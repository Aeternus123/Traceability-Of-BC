#!/usr/bin/env python3
import sys

import rospkg
import rospy
import cv2
import torch
import numpy as np
import os
import time
import math
import threading
import yaml
import json
import requests
import base64
from cv_bridge import CvBridge
from sensor_msgs.msg import Image
from std_msgs.msg import Bool, Int8, Float32
import message_filters

# 添加Flask库用于HTTP服务器
from flask import Flask, request, jsonify
import threading

# 自定义消息
from dofbot_pro_info.msg import Image_Msg, Yolov5Detect, ArmJoint
from dofbot_pro_info.srv import kinemarics, kinemaricsRequest

# === 在导入任何包之前设置环境变量 ===
os.environ['ULTRALYTICS_AUTOUPDATE'] = 'false'
os.environ['YOLO_VERBOSE'] = 'false'
os.environ['YOLO_DOWNLOAD'] = 'false'

import warnings
warnings.filterwarnings('ignore')

# YOLOv11相关导入
try:
    from ultralytics import YOLO
    print("✅ Ultralytics导入成功")
except Exception as e:
    print(f"❌ Ultralytics导入失败: {e}")
    sys.exit(1)

class YOLOv11ArmControl:
    def __init__(self):
        # 等待ROS master
        self.wait_for_ros_master()
        
        # ROS节点初始化
        rospy.init_node('yolov11_arm_control')
        
        # 参数初始化
        self.init_parameters()
        
        # 模型初始化
        self.init_model()
        
        # ROS组件初始化
        self.init_ros_components()
        
        # 机械臂参数初始化
        self.init_arm_parameters()
        
        # 状态变量
        self.pr_time = time.time()
        self.start_flag = False
        self.start_sort = False
        self.grasp_flag = True
        self.dist = 0
        self.cx = 320
        self.cy = 240
        self.name = None
        
        # 初始化Flask应用
        self.flask_app = Flask(__name__)
        self.setup_flask_routes()
        
        print("YOLOv11 Arm Control System Initialized Complete!")
        print(f"Current End Pose: {self.CurEndPos}")
        print(f"Offsets - X: {self.x_offset}, Y: {self.y_offset}, Z: {self.z_offset}")
        print("HTTP服务器已初始化，将在单独线程中运行")
        
        # 在单独线程中启动Flask服务器
        self.flask_thread = threading.Thread(target=self.run_flask_server, daemon=True)
        self.flask_thread.start()

    def wait_for_ros_master(self):
        """等待ROS master启动"""
        print("等待ROS master启动...")
        max_attempts = 30
        for i in range(max_attempts):
            try:
                import rosgraph
                if rosgraph.is_master_online():
                    print("✅ ROS master连接成功")
                    return
                else:
                    print(f"⏳ 等待ROS master... ({i+1}/{max_attempts})")
            except Exception as e:
                print(f"⏳ ROS master未就绪... ({i+1}/{max_attempts})")
            time.sleep(1)
        
        print("❌ 无法连接到ROS master，请确保roscore正在运行")
        print("运行命令: roscore")
        sys.exit(1)

    def init_parameters(self):
        """初始化参数"""
        # 图像参数
        self.img_width = 640
        self.img_height = 480
        self.img_flip = rospy.get_param("~img_flip", False)
        
        # YOLOv11参数
        self.confidence_threshold = 0.8
        self.iou_threshold = 0.5
        
        # 加载偏移量参数
        offset_file = rospkg.RosPack().get_path("dofbot_pro_info") + "/param/offset_value.yaml"
        with open(offset_file, 'r') as file:
            offset_config = yaml.safe_load(file)
        
        self.x_offset = offset_config.get('x_offset', 0.0)
        self.y_offset = offset_config.get('y_offset', 0.0)
        self.z_offset = offset_config.get('z_offset', 0.0)

    def init_model(self):
        """初始化YOLOv11模型"""
        try:
            # 加载YOLOv11模型
            model_path = rospy.get_param("~model_path", "/home/jetson/Desktop/test/impurity.pt")
            
            if not os.path.exists(model_path):
                print(f"❌ 模型文件不存在: {model_path}")
                sys.exit(1)
                
            self.model = YOLO(model_path)
            print(f"✅ YOLOv11模型加载成功: {model_path}")
            
            # 获取类别名称
            self.names = self.model.names
            print(f"检测类别: {list(self.names.values())}")
            
        except Exception as e:
            rospy.logerr(f"Failed to load YOLOv11 model: {e}")
            sys.exit(1)

    def init_ros_components(self):
        """初始化ROS组件"""
        self.bridge = CvBridge()
        
        # 发布者
        self.image_pub = rospy.Publisher('/image_data', Image_Msg, queue_size=1)
        self.pub_point = rospy.Publisher("TargetAngle", ArmJoint, queue_size=1)
        self.pub_detect = rospy.Publisher("Yolov5DetectInfo", Yolov5Detect, queue_size=1)
        # 设置main.go服务器地址
        self.main_go_server_url = "http://localhost:5500/api/detect"
        # 添加配置选项，控制是否启用向main.go发送检测结果的功能
        self.enable_send_to_main_go = rospy.get_param("~enable_send_to_main_go", True)
        rospy.loginfo(f"向main.go发送检测结果功能已{'启用' if self.enable_send_to_main_go else '禁用'}")
        self.pub_sort_flag = rospy.Publisher('sort_flag', Bool, queue_size=1)
        self.pub_grasp_status = rospy.Publisher("grasp_done", Bool, queue_size=1)
        self.pub_play_id = rospy.Publisher("player_id", Int8, queue_size=1)
        
        # 订阅者
        self.image_sub = rospy.Subscriber("/camera/color/image_raw", Image, self.image_callback)
        self.depth_image_sub = rospy.Subscriber('/camera/depth/image_raw', Image, self.depth_callback)
        self.grasp_status_sub = rospy.Subscriber('grasp_done', Bool, self.grasp_status_callback)
        
        # 服务客户端
        try:
            rospy.wait_for_service("get_kinemarics", timeout=10)
            self.kinematics_client = rospy.ServiceProxy("get_kinemarics", kinemarics)
            rospy.loginfo("✅ 运动学服务连接成功")
        except rospy.ROSException:
            rospy.logwarn("⚠️ 运动学服务未就绪，将继续等待...")
        
        # 设置Flask服务器端口
        self.flask_port = 8000

    def init_arm_parameters(self):
        """初始化机械臂参数"""
        # 关节角度
        self.init_joints = [90.0, 120, 0.0, 0.0, 90, 90]
        self.down_joint = [130.0, 55.0, 34.0, 16.0, 90.0, 125]
        self.set_joint = [90.0, 120, 0.0, 0.0, 90, 90]
        self.gripper_joint = 90
        
        # 相机参数
        self.camera_info_K = [477.57421875, 0.0, 319.3820495605469, 0.0, 477.55718994140625, 238.64108276367188, 0.0, 0.0, 1.0]
        
        # 坐标变换矩阵
        self.EndToCamMat = np.array([
            [1.00000000e+00, 0.00000000e+00, 0.00000000e+00, 0.00000000e+00],
            [0.00000000e+00, 7.96326711e-04, 9.99999683e-01, -9.90000000e-02],
            [0.00000000e+00, -9.99999683e-01, 7.96326711e-04, 4.90000000e-02],
            [0.00000000e+00, 0.00000000e+00, 0.00000000e+00, 1.00000000e+00]
        ])
        
        # 当前位置
        self.CurEndPos = [0.0, 0.0, 0.0, 0.0, 0.0, 0.0]
        self.get_current_end_pos()
        
        # 垃圾分类
        self.recyclable_waste = ['Newspaper','Zip_top_can','Book','Old_school_bag']
        self.toxic_waste = ['Syringe','Expired_cosmetics','Used_batteries','Expired_tablets']
        self.wet_waste = ['Fish_bone','Egg_shell','Apple_core','Watermelon_rind']
        self.dry_waste = ['Toilet_paper','Peach_pit','Cigarette_butts','Disposable_chopsticks']

    def get_current_end_pos(self):
        """获取当前机械臂末端位置（简化实现）"""
        # 这里需要根据实际的机械臂状态获取方法实现
        self.CurEndPos = [0.2, 0.0, 0.3, 0.0, 0.0, 0.0]  # 示例值

    def image_callback(self, data):
        """彩色图像回调函数"""
        try:
            # 创建检测结果对象
            detect_result = {
                "success": False,
                "message": "",
                "detections": [],
                "detected_image_path": ""
            }
            
            # 1. 转换图像消息
            cv_image = self.bridge.imgmsg_to_cv2(data, "bgr8")
            
            if self.img_flip:
                cv_image = cv2.flip(cv_image, 1)
            
            # 发布图像数据
            image_msg = Image_Msg()
            image_msg.height = cv_image.shape[0]
            image_msg.width = cv_image.shape[1]
            image_msg.channels = cv_image.shape[2]
            image_msg.data = data.data
            self.image_pub.publish(image_msg)
            
            # 2. YOLOv11目标检测
            results = self.model(cv_image, conf=self.confidence_threshold, iou=self.iou_threshold)
            
            # 处理检测结果
            detected = False
            for result in results:
                boxes = result.boxes
                if boxes is not None:
                    for box in boxes:
                        conf = box.conf.item()
                        cls = int(box.cls.item())
                        xyxy = box.xyxy[0].cpu().numpy()
                        
                        # 绘制检测框
                        label = f'{self.names[cls]} {conf:.2f}'
                        self.plot_box(cv_image, xyxy, label, cls)
                        
                        # 检测到高置信度目标且允许分拣
                        if conf > self.confidence_threshold and self.start_flag:
                            cx = (xyxy[0] + xyxy[2]) / 2
                            cy = (xyxy[1] + xyxy[3]) / 2
                            
                            # 发布检测信息
                            detect_info = Yolov5Detect()
                            detect_info.centerx = cx
                            detect_info.centery = cy
                            detect_info.result = self.names[cls]
                            self.pub_detect.publish(detect_info)  # 修复缩进
                            
                            self.cx = int(cx)
                            self.cy = int(cy)
                            self.name = self.names[cls]
                            detected = True
                            
                            # 添加到检测结果列表
                            x1, y1, x2, y2 = map(int, xyxy)
                            bbox = [x1, y1, x2, y2]
                            detect_result["detections"].append({
                                "class": self.names[cls],
                                "confidence": float(conf),
                                "bbox": bbox
                            })
                            self.start_flag = False
                            break
                
                if detected:
                    break
            
            # 计算并显示帧率
            cur_time = time.time()
            fps = str(int(1/(cur_time - self.pr_time)))
            self.pr_time = cur_time
            cv2.putText(cv_image, f"FPS: {fps}", (10, 30), cv2.FONT_HERSHEY_SIMPLEX, 1, (0, 255, 0), 2)
            
            # 显示图像
            cv2.imshow("YOLOv11 Detection", cv_image)
            
            # 如果有检测结果，发送到main.go
            if detected or len(detect_result["detections"]) > 0:
                detect_result["success"] = True
                detect_result["message"] = f"成功检测到 {len(detect_result['detections'])} 个对象"
                self.send_detection_result_to_main_go(cv_image)
            
            # 检查按键
            key = cv2.waitKey(1) & 0xFF
            if key == 32:  # 空格键
                self.start_flag = True
                self.publish_sort_flag(True)
            
        except Exception as e:
            rospy.logerr(f"Error in image callback: {e}")

    def depth_callback(self, data):
        """深度图像回调函数"""
        try:
            if self.cy == 0 or self.cx == 0 or self.name is None:
                return
                
            # 转换深度图像
            depth_image = self.bridge.imgmsg_to_cv2(data, "32FC1")
            depth_image = cv2.resize(depth_image, (self.img_width, self.img_height))
            
            # 获取深度值
            self.dist = depth_image[self.cy, self.cx] / 1000.0
            
            if self.dist != 0 and self.name is not None and self.start_sort:
                rospy.loginfo(f"Detected: {self.name} at ({self.cx}, {self.cy}), depth: {self.dist:.3f}m")
                
                # 坐标转换和夹取
                grasp_thread = threading.Thread(target=self.process_grasping)
                grasp_thread.start()
                
        except Exception as e:
            rospy.logerr(f"Error in depth callback: {e}")

    def process_grasping(self):
        """处理夹取流程"""
        try:
            # 坐标转换
            camera_location = self.pixel_to_camera_depth((self.cx, self.cy), self.dist)
            world_pose = self.transform_to_world_coordinates(camera_location)
            
            # 执行夹取
            self.execute_grasp(world_pose)
            
        except Exception as e:
            rospy.logerr(f"Error in grasping process: {e}")

    def pixel_to_camera_depth(self, pixel, depth):
        """像素坐标转相机坐标"""
        u, v = pixel
        fx = self.camera_info_K[0]
        fy = self.camera_info_K[4]
        cx = self.camera_info_K[2]
        cy = self.camera_info_K[5]
        
        x = (u - cx) * depth / fx
        y = (v - cy) * depth / fy
        z = depth
        
        return [x, y, z]

    def transform_to_world_coordinates(self, camera_location):
        """转换到世界坐标系"""
        # 相机坐标转末端坐标
        PoseEndMat = np.matmul(self.EndToCamMat, self.xyz_euler_to_mat(camera_location, (0, 0, 0)))
        
        # 末端坐标转世界坐标
        EndPointMat = self.get_end_point_mat()
        WorldPose = np.matmul(EndPointMat, PoseEndMat)
        
        pose_T, pose_R = self.mat_to_xyz_euler(WorldPose)
        
        # 应用偏移补偿
        pose_T[0] += self.x_offset
        pose_T[1] += self.y_offset
        pose_T[2] += self.z_offset
        
        return pose_T

    def execute_grasp(self, target_pose):
        """执行夹取动作"""
        try:
            # 确保运动学服务可用
            if self.kinematics_client is None:
                rospy.wait_for_service("get_kinemarics")
                self.kinematics_client = rospy.ServiceProxy("get_kinemarics", kinemarics)
            
            # 调用逆运动学服务
            request = kinemaricsRequest()
            request.tar_x = target_pose[0]
            request.tar_y = target_pose[1]
            request.tar_z = target_pose[2] + (math.sqrt(request.tar_y**2 + request.tar_x**2) - 0.181) * 0.2
            request.kin_name = "ik"
            request.Roll = self.CurEndPos[3]
            
            response = self.kinematics_client(request)
            
            # 计算关节角度
            joints = [0.0] * 6
            joints[0] = response.joint1
            joints[1] = response.joint2
            joints[2] = response.joint3
            joints[3] = min(response.joint4, 90)  # 限制关节4角度
            joints[4] = 90
            joints[5] = 30
            
            rospy.loginfo(f"Moving to joints: {joints}")
            
            # 发布目标角度
            self.publish_target_angles(joints)
            
            # 等待运动完成
            time.sleep(2.5)
            
            # 执行分类放置
            self.classify_and_place()
            
        except Exception as e:
            rospy.logerr(f"Grasping execution failed: {e}")

    def classify_and_place(self):
        """根据分类结果放置物体"""
        if self.name in self.recyclable_waste:
            self.place_recyclable()
        elif self.name in self.toxic_waste:
            self.place_toxic()
        elif self.name in self.wet_waste:
            self.place_wet()
        elif self.name in self.dry_waste:
            self.place_dry()
        else:
            rospy.logwarn(f"Unknown object type: {self.name}")
        
        # 发布夹取完成信号
        self.publish_grasp_status(True)
        
        # 重置状态
        self.reset_detection_state()

    def place_recyclable(self):
        """放置可回收垃圾"""
        rospy.loginfo("Placing in recyclable bin")
        self.publish_voice_id(1)  # 语音提示ID

    def place_toxic(self):
        """放置有害垃圾"""
        rospy.loginfo("Placing in toxic bin")
        self.publish_voice_id(2)

    def place_wet(self):
        """放置湿垃圾"""
        rospy.loginfo("Placing in wet bin")
        self.publish_voice_id(3)

    def place_dry(self):
        """放置干垃圾"""
        rospy.loginfo("Placing in dry bin")
        self.publish_voice_id(4)

    def send_detection_result_to_main_go(self, image):
        """发送检测结果图像到main.go服务器"""
        if not self.enable_send_to_main_go:
            return
            
        try:
            # 将OpenCV图像转换为字节
            _, img_encoded = cv2.imencode('.jpg', image)
            img_bytes = img_encoded.tobytes()
            
            # 创建multipart/form-data请求
            files = {'image': ('detected_image.jpg', img_bytes, 'image/jpeg')}
            
            # 发送POST请求
            rospy.loginfo(f"正在将检测结果图像发送到main.go服务器: {self.main_go_server_url}")
            
            response = requests.post(
                self.main_go_server_url,
                files=files,
                timeout=10
            )
            
            # 检查响应状态
            if response.status_code == 200:
                rospy.loginfo(f"成功发送检测结果图像到main.go")
            else:
                rospy.logerr(f"发送失败，状态码: {response.status_code}")
        
        except Exception as e:
            rospy.logerr(f"发送检测结果图像到main.go时出错: {str(e)}")

    def reset_detection_state(self):
        """重置检测状态"""
        self.cx = 320
        self.cy = 240
        self.name = None
        self.start_sort = False

    def grasp_status_callback(self, msg):
        """夹取状态回调"""
        self.grasp_flag = msg.data

    def publish_sort_flag(self, flag):
        """发布分拣标志"""
        msg = Bool()
        msg.data = flag
        self.pub_sort_flag.publish(msg)
        self.start_sort = flag

    def publish_grasp_status(self, status):
        """发布夹取状态"""
        msg = Bool()
        msg.data = status
        self.pub_grasp_status.publish(msg)

    def publish_voice_id(self, voice_id):
        """发布语音ID"""
        msg = Int8()
        msg.data = voice_id
        self.pub_play_id.publish(msg)

    def publish_target_angles(self, joints):
        """发布目标关节角度"""
        msg = ArmJoint()
        msg.joint = joints
        self.pub_point.publish(msg)

    def plot_box(self, img, xyxy, label, cls_id):
        """绘制检测框"""
        color = self.get_color(cls_id)
        x1, y1, x2, y2 = map(int, xyxy)
        
        cv2.rectangle(img, (x1, y1), (x2, y2), color, 2)
        
        # 绘制标签背景
        label_size = cv2.getTextSize(label, cv2.FONT_HERSHEY_SIMPLEX, 0.5, 2)[0]
        cv2.rectangle(img, (x1, y1 - label_size[1] - 10), 
                     (x1 + label_size[0], y1), color, -1)
        
        # 绘制标签文字
        cv2.putText(img, label, (x1, y1 - 5), 
                   cv2.FONT_HERSHEY_SIMPLEX, 0.5, (255, 255, 255), 2)


    # 坐标转换工具函数
    def xyz_euler_to_mat(self, translation, euler):
        """位姿转变换矩阵"""
        mat = np.eye(4)
        mat[0:3, 3] = translation
        return mat

    def mat_to_xyz_euler(self, mat):
        """变换矩阵转位姿"""
        translation = mat[0:3, 3]
        rotation = [0, 0, 0]
        return translation, rotation

    def get_end_point_mat(self):
        """获取末端位姿矩阵"""
        return np.eye(4)

    def setup_flask_routes(self):
        """设置Flask API路由"""
        @self.flask_app.route('/api/detect', methods=['POST'])
        def detect_endpoint():
            try:
                # 检查是否有文件上传
                if 'file' not in request.files:
                    return jsonify({'error': 'No file part'}), 400
                
                file = request.files['file']
                if file.filename == '':
                    return jsonify({'error': 'No selected file'}), 400
                
                # 读取图像文件
                img_data = file.read()
                nparr = np.frombuffer(img_data, np.uint8)
                img = cv2.imdecode(nparr, cv2.IMREAD_COLOR)
                
                if img is None:
                    return jsonify({'error': 'Invalid image format'}), 400
                
                # 执行YOLO检测
                results = self.model(img, conf=self.conf_thres, iou=self.iou_thres)
                
                # 处理检测结果
                detections = []
                for result in results:
                    boxes = result.boxes
                    for box in boxes:
                        # 提取边界框坐标和置信度
                        x1, y1, x2, y2 = box.xyxy[0].tolist()
                        conf = box.conf[0].item()
                        cls_id = box.cls[0].item()
                        
                        # 获取类别名称
                        cls_name = result.names[int(cls_id)]
                        
                        # 计算中心点坐标
                        center_x = (x1 + x2) / 2
                        center_y = (y1 + y2) / 2
                        
                        detections.append({
                            'class_id': int(cls_id),
                            'class_name': cls_name,
                            'confidence': conf,
                            'x1': x1,
                            'y1': y1,
                            'x2': x2,
                            'y2': y2,
                            'center_x': center_x,
                            'center_y': center_y
                        })
                
                # 返回检测结果
                return jsonify({
                    'status': 'success',
                    'detections': detections,
                    'image_width': img.shape[1],
                    'image_height': img.shape[0]
                })
                
            except Exception as e:
                rospy.logerr(f"处理检测请求时出错: {str(e)}")
                return jsonify({'error': str(e)}), 500
    
    def run_flask_server(self):
        """在单独线程中运行Flask服务器"""
        try:
            rospy.loginfo(f"启动Flask服务器，监听端口 {self.flask_port}")
            # 使用threaded=True允许处理多个并发请求
            self.flask_app.run(host='0.0.0.0', port=self.flask_port, threaded=True)
        except Exception as e:
            rospy.logerr(f"Flask服务器启动失败: {str(e)}")
    
    def run(self):
        """主运行循环"""
        rospy.loginfo("YOLOv11 Arm Control System Started!")
        try:
            rospy.spin()
        except KeyboardInterrupt:
            rospy.loginfo("Shutting down...")
        finally:
            cv2.destroyAllWindows()

if __name__ == '__main__':
    try:
        controller = YOLOv11ArmControl()
        controller.run()
    except rospy.ROSInterruptException:
        pass