/* FFmpeg 8.0.1 绑定入口：只暴露探测、解码和 RGB24 转换所需声明。 */
#include <libavutil/avutil.h>
#include <libavutil/frame.h>
#include <libavutil/pixfmt.h>
#include <libavcodec/avcodec.h>
#include <libavcodec/packet.h>
#include <libavformat/avformat.h>
#include <libswscale/swscale.h>
