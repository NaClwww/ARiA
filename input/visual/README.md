# 视觉处理部分

本质就是一个一直循环跑的模块，通过hook之类的将处理过后易于理解的图片交给Agent Loop

1. 生成对应热力图
    1. [saliency prediction](https://huggingface.co/spaces/alexanderkroner/saliency)
        静态图片处理
    2. 快速移动物体捕捉
2. 图片处理：有明显中心时将周围裁剪+降不透明度；没有明显中心降分辨率
3. 
4. 推送到主Agent的sight context

# 主Agent
通过tools控制是否开启sight context（暂定）