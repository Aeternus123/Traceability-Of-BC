from flask import Flask, request, jsonify
import cv2
import numpy as np
import base64
import requests
from ultralytics import YOLO
import io
from PIL import Image
from flask_cors import CORS
import logging
from logging.handlers import RotatingFileHandler
import os
import time
import uuid
from functools import wraps
import re

# 初始化Flask应用
app = Flask(__name__)

# 配置CORS，允许所有来源访问
ALLOWED_ORIGINS = os.environ.get('ALLOWED_ORIGINS', '*').split(',')
CORS(app, resources={
    r"/*": {
        "origins": ALLOWED_ORIGINS,
        "methods": ["GET", "POST", "OPTIONS"],
        "allow_headers": ["Content-Type", "Authorization"]
    }
})

# 定义检测结果保存路径（与Go服务保持一致）
DETECTIONS_DIR = '/home/ubuntu/.product-chain/detections/'
os.makedirs(DETECTIONS_DIR, exist_ok=True)

# Go服务器地址
GO_SERVER_URL = os.environ.get('GO_SERVER_URL', 'http://localhost:8090')

# 配置日志
if not os.path.exists('logs'):
    os.mkdir('logs')
file_handler = RotatingFileHandler('logs/yolodetect.log', maxBytes=10240, backupCount=10)
file_handler.setFormatter(logging.Formatter(
    '%(asctime)s %(levelname)s: %(message)s [in %(pathname)s:%(lineno)d]'
))
file_handler.setLevel(logging.INFO)
app.logger.addHandler(file_handler)
app.logger.setLevel(logging.INFO)
app.logger.info('YOLO detection server startup')

# 限制请求大小
app.config['MAX_CONTENT_LENGTH'] = 16 * 1024 * 1024  # 16MB

# 加载YOLO模型
model1 = YOLO('EM.pt') 
model2 = YOLO('impurity.pt') 


# 性能计时装饰器
def timing_decorator(f):
    @wraps(f)
    def wrapper(*args, **kwargs):
        start_time = time.time()
        result = f(*args, **kwargs)
        end_time = time.time()
        app.logger.info(f"Request processed in {end_time - start_time:.4f} seconds")
        return result

    return wrapper


def validate_block_level(level):
    """验证区块链等级是否合法（0-7）"""
    try:
        level_int = int(level)
        return 0 <= level_int <= 7, level_int
    except (ValueError, TypeError):
        return False, 0


def is_multipart_form_data(content_type):
    """检查内容类型是否为multipart/form-data"""
    return content_type and content_type.startswith('multipart/form-data')


def clean_batch_id(batch_id):
    """
    清洗批次ID，确保格式简单统一
    移除特殊字符，保留字母、数字和下划线
    """
    if not batch_id:
        return None

    # 移除所有非字母数字下划线的字符
    cleaned = re.sub(r'[^\w]', '', str(batch_id))

    # 确保批次ID不为空
    if not cleaned:
        return None

    return cleaned


def send_to_go_server(batch_id, block_level, image_path, detection_hash, detections):
    """将检测结果发送到Go服务器"""
    try:
        # 准备发送到Go服务器的数据
        payload = {
            "batch_id": batch_id,
            "block_level": block_level,
            "detection_id": str(uuid.uuid4()),
            "success": True,
            "message": f"Detected {len(detections)} objects",
            "image_path": image_path,
            "detection_hash": detection_hash,
            "detections": detections
        }

        # 发送POST请求到Go服务器的回调接口
        response = requests.post(
            f"{GO_SERVER_URL}/api/yolo/callback",
            json=payload,
            timeout=10
        )

        if response.status_code == 200:
            app.logger.info(f"Successfully sent detection results to Go server for batch {batch_id}")
            return True, "Results sent to blockchain server"
        else:
            app.logger.error(f"Failed to send results to Go server. Status code: {response.status_code}")
            return False, f"Blockchain server returned status code: {response.status_code}"

    except Exception as e:
        app.logger.error(f"Error sending results to Go server: {str(e)}")
        return False, f"Error communicating with blockchain server: {str(e)}"


@app.route('/detect', methods=['POST'])
@timing_decorator
def detect():
    try:
        # 获取核心参数：批次号和区块链等级
        batch_id = request.form.get('batch_id') or request.args.get('batch_id')
        block_level_str = request.form.get('block_level') or request.args.get('block_level')

        # 验证核心参数
        if not batch_id:
            app.logger.warning("No batch_id provided")
            return jsonify({'success': False, 'error': 'Missing required parameter: batch_id'}), 400

        # 清洗批次ID，确保格式简单统一
        batch_id = clean_batch_id(batch_id)
        if not batch_id:
            app.logger.warning("Invalid batch_id provided after cleaning")
            return jsonify({'success': False,
                            'error': 'Invalid batch_id format: only letters, numbers and underscores allowed'}), 400

        is_valid_level, block_level = validate_block_level(block_level_str)
        if not is_valid_level:
            app.logger.warning(f"Invalid block_level: {block_level_str}")
            return jsonify({'success': False, 'error': 'Invalid block_level: must be integer between 0 and 7'}), 400

        # 处理图像数据
        image = None
        content_type = request.content_type

        if is_multipart_form_data(content_type) and 'image' in request.files:
            image_file = request.files['image']
            allowed_extensions = {'png', 'jpg', 'jpeg', 'gif'}
            if '.' not in image_file.filename or \
                    image_file.filename.rsplit('.', 1)[1].lower() not in allowed_extensions:
                app.logger.warning(f"Invalid file extension: {image_file.filename}")
                return jsonify({'success': False, 'error': 'Invalid file type: only png/jpg/jpeg/gif allowed'}), 400
            image = Image.open(image_file)

        elif request.data:
            image = Image.open(io.BytesIO(request.data))

        else:
            app.logger.warning("No image data provided")
            return jsonify({'success': False, 'error': 'No image data provided'}), 400

        # 图像预处理
        image_np = np.array(image)
        max_dimension = 1024
        h, w = image_np.shape[:2]
        if max(h, w) > max_dimension:
            ratio = max_dimension / max(h, w)
            new_size = (int(w * ratio), int(h * ratio))
            image_np = cv2.resize(image_np, new_size)
            app.logger.info(f"Resized image from ({w}, {h}) to {new_size}")

        # 转换为BGR格式
        if len(image_np.shape) == 3:
            image_np = cv2.cvtColor(image_np, cv2.COLOR_RGB2BGR)

        # 根据批次处理等级决定检测模型
        start_detection = time.time()
        results1 = None
        results2 = None
        detection_models = []
        
        if block_level == 0:
            # 第0级：批次注册，两个模型都检测
            results1 = model1(image_np, conf=0.3)
            results2 = model2(image_np, conf=0.3)
            detection_models = ['model1', 'model2']
        elif block_level == 1:
            # 第1级：原材料登记，仅model2检测
            results2 = model2(image_np, conf=0.3)
            detection_models = ['model2']
        elif 2 <= block_level <= 4:
            # 第2-4级：仅model1检测
            results1 = model1(image_np, conf=0.3)
            detection_models = ['model1']
        # 第5-6级：不进行模型检测
        
        detection_time = time.time() - start_detection
        app.logger.info(
            f"Detection completed for batch {batch_id} (level {block_level}, models: {', '.join(detection_models)}) in {detection_time:.4f} seconds")

        # 解析检测结果
        detections = []
        annotated_image = image_np.copy()

        # 定义不同模型的框颜色，以便区分
        colors = [(0, 255, 0), (0, 0, 255)]  # 绿色(模型1), 蓝色(模型2)
        
        # 处理第一个模型的结果
        if results1 is not None:
            for result in results1:
                for box in result.boxes:
                    x1, y1, x2, y2 = map(int, box.xyxy[0].tolist())
                    conf = box.conf[0].item()
                    cls = int(box.cls[0].item())
                    class_name = model1.names[cls]

                    detections.append({
                        'class': class_name,
                        'confidence': round(conf, 4),
                        'x1': x1,
                        'y1': y1,
                        'x2': x2,
                        'y2': y2,
                        'model': 'model1'
                    })

                    # 绘制检测框（绿色表示来自第一个模型）
                    cv2.rectangle(annotated_image, (x1, y1), (x2, y2), colors[0], 2)
                    label = f'm1:{class_name}: {conf:.2f}'
                    cv2.putText(annotated_image, label, (x1, y1 - 10),
                                cv2.FONT_HERSHEY_SIMPLEX, 0.5, colors[0], 2)
        
        # 处理第二个模型的结果
        if results2 is not None:
            for result in results2:
                for box in result.boxes:
                    x1, y1, x2, y2 = map(int, box.xyxy[0].tolist())
                    conf = box.conf[0].item()
                    cls = int(box.cls[0].item())
                    class_name = model2.names[cls]
                    
                    detections.append({
                        'class': class_name,
                        'confidence': round(conf, 4),
                        'x1': x1,
                        'y1': y1,
                        'x2': x2,
                        'y2': y2,
                        'model': 'model2'
                    })
                    
                    # 绘制检测框（蓝色表示来自第二个模型）
                    cv2.rectangle(annotated_image, (x1, y1), (x2, y2), colors[1], 2)
                    label = f'm2:{class_name}: {conf:.2f}'
                    cv2.putText(annotated_image, label, (x1, y1 - 10),
                                cv2.FONT_HERSHEY_SIMPLEX, 0.5, colors[1], 2)
        
        # 第5-6级：不进行检测，但仍需保存图片
        if block_level in [5, 6]:
            app.logger.info(f"No detection performed for batch {batch_id} (level {block_level}), saving original image")

        # 保存检测结果图片（使用简单批次ID+等级命名）
        output_filename = f"{batch_id}_{block_level}.jpg"
        output_path = os.path.join(DETECTIONS_DIR, output_filename)

        # 转换回RGB格式并保存
        annotated_image_rgb = cv2.cvtColor(annotated_image, cv2.COLOR_BGR2RGB)
        cv2.imwrite(output_path, annotated_image_rgb, [int(cv2.IMWRITE_JPEG_QUALITY), 80])
        app.logger.info(f"Saved detection image to: {output_path}")

        # 计算图片哈希（与Go服务保持一致）
        image_hash = base64.b64encode(cv2.imencode('.jpg', annotated_image_rgb)[1]).hex()[:64]

        # 编码为Base64返回
        success, encoded_image = cv2.imencode('.jpg', annotated_image_rgb, [int(cv2.IMWRITE_JPEG_QUALITY), 80])
        if not success:
            app.logger.error(f"Failed to encode image for batch: {batch_id}")
            return jsonify({'success': False, 'error': 'Failed to encode detection image'}), 500

        base64_image = base64.b64encode(encoded_image).decode('utf-8')

        # 发送结果到Go服务器
        go_success, go_message = send_to_go_server(
            batch_id,
            block_level,
            output_path,
            image_hash,
            detections
        )

        # 返回结果给客户端
        # 根据检测情况记录日志
        if block_level in [5, 6]:
            app.logger.info(f"Successfully processed batch {batch_id} (level {block_level}): no detection performed, image saved")
        else:
            app.logger.info(
                f"Successfully processed detection for batch {batch_id} (level {block_level}): {len(detections)} objects found")
        return jsonify({
            'success': True,
            'detections': detections,
            'image': base64_image,
            'batch_id': batch_id,  # 返回清洗后的批次ID
            'block_level': block_level,
            'saved_filename': output_filename,
            'saved_path': output_path,
            'image_hash': image_hash,
            'blockchain_status': {
                'success': go_success,
                'message': go_message
            }
        })

    except Exception as e:
        app.logger.error(f"Error processing detection request: {str(e)}", exc_info=True)
        return jsonify({'success': False, 'error': 'Internal server error'}), 500


@app.route('/health', methods=['GET'])
def health_check():
    """健康检查接口，供区块链服务监控"""
    model_available = model1 is not None and model2 is not None
    dir_available = os.path.exists(DETECTIONS_DIR) and os.access(DETECTIONS_DIR, os.W_OK)

    # 检查与Go服务器的连接
    go_server_reachable = False
    try:
        response = requests.get(f"{GO_SERVER_URL}/health", timeout=5)
        go_server_reachable = response.status_code == 200
    except:
        pass

    if model_available and dir_available and go_server_reachable:
        return jsonify({
            'status': 'healthy',
            'timestamp': time.time(),
            'models': ['EM.pt', 'impurity.pt'],
            'storage': 'available',
            'blockchain_server': 'reachable'
        }), 200
    else:
        return jsonify({
            'status': 'unhealthy',
            'timestamp': time.time(),
            'model': 'unavailable' if not model_available else 'available',
            'storage': 'unavailable' if not dir_available else 'available',
            'blockchain_server': 'unreachable' if not go_server_reachable else 'reachable'
        }), 503


if __name__ == '__main__':
    port = int(os.environ.get('PORT', 8000))
    app.run(host='0.0.0.0', port=port, debug=False)
