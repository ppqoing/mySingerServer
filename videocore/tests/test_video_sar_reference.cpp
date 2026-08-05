#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <bcrypt.h>

#include <array>
#include <algorithm>
#include <cstdint>
#include <cstring>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <memory>
#include <sstream>
#include <string>
#include <vector>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/avutil.h>
#include <libavutil/mathematics.h>
#include <libswscale/swscale.h>
}

#include "native_algorithms/gray_image.h"
#include "native_algorithms/pdq.h"
#include "native_algorithms/phash_parts.h"
#include "native_algorithms/sobel_hist.h"

namespace {

using videocore::native::GrayImage;
using videocore::native::ImageStatus;

template <typename T, void (*Free)(T**)>
struct AvOwner {
    T* value = nullptr;
    ~AvOwner() { Free(&value); }
};

void FreeFormat(AVFormatContext** value) {
    if (*value != nullptr) avformat_close_input(value);
}

std::string HexBytes(const uint8_t* bytes, size_t size) {
    std::ostringstream output;
    output << std::hex << std::setfill('0');
    for (size_t i = 0; i < size; ++i) {
        output << std::setw(2) << unsigned(bytes[i]);
    }
    return output.str();
}

std::string Sha256File(const char* path) {
    BCRYPT_ALG_HANDLE algorithm = nullptr;
    BCRYPT_HASH_HANDLE hash = nullptr;
    DWORD object_size = 0;
    DWORD result_size = 0;
    std::vector<uint8_t> object;
    std::array<uint8_t, 32> digest{};
    if (BCryptOpenAlgorithmProvider(
            &algorithm, BCRYPT_SHA256_ALGORITHM, nullptr, 0) < 0 ||
        BCryptGetProperty(algorithm,
                          BCRYPT_OBJECT_LENGTH,
                          reinterpret_cast<PUCHAR>(&object_size),
                          sizeof(object_size),
                          &result_size,
                          0) < 0) {
        if (algorithm != nullptr) BCryptCloseAlgorithmProvider(algorithm, 0);
        return {};
    }
    object.resize(object_size);
    if (BCryptCreateHash(algorithm,
                         &hash,
                         object.data(),
                         object_size,
                         nullptr,
                         0,
                         0) < 0) {
        BCryptCloseAlgorithmProvider(algorithm, 0);
        return {};
    }
    std::ifstream input(path, std::ios::binary);
    std::array<uint8_t, 65536> buffer{};
    while (input) {
        input.read(reinterpret_cast<char*>(buffer.data()), buffer.size());
        const auto count = input.gcount();
        if (count > 0 &&
            BCryptHashData(hash,
                           buffer.data(),
                           static_cast<ULONG>(count),
                           0) < 0) {
            BCryptDestroyHash(hash);
            BCryptCloseAlgorithmProvider(algorithm, 0);
            return {};
        }
    }
    const bool ok = input.eof() &&
                    BCryptFinishHash(hash,
                                     digest.data(),
                                     static_cast<ULONG>(digest.size()),
                                     0) >= 0;
    BCryptDestroyHash(hash);
    BCryptCloseAlgorithmProvider(algorithm, 0);
    return ok ? HexBytes(digest.data(), digest.size()) : std::string{};
}

bool ScaleSarGray(AVFormatContext* format,
                  AVStream* stream,
                  const AVFrame* frame,
                  GrayImage* gray) {
    AVRational sar = av_guess_sample_aspect_ratio(
        format, stream, const_cast<AVFrame*>(frame));
    if (sar.num <= 0 || sar.den <= 0) sar = AVRational{1, 1};
    const long double display_width =
        static_cast<long double>(frame->width) * sar.num / sar.den;
    const long double display_height = frame->height;
    int width = 0;
    int height = 0;
    if (display_width >= display_height) {
        width = 512;
        height = (std::max)(1, static_cast<int>(
            512.0L * display_height / display_width));
    } else {
        height = 512;
        width = (std::max)(1, static_cast<int>(
            512.0L * display_width / display_height));
    }
    std::unique_ptr<SwsContext, decltype(&sws_freeContext)> scaler(
        sws_getContext(frame->width,
                       frame->height,
                       static_cast<AVPixelFormat>(frame->format),
                       width,
                       height,
                       AV_PIX_FMT_GRAY8,
                       SWS_BICUBIC,
                       nullptr,
                       nullptr,
                       nullptr),
        &sws_freeContext);
    if (!scaler) return false;
    gray->width = width;
    gray->height = height;
    gray->stride = width;
    gray->pixels.resize(static_cast<size_t>(width) * height);
    uint8_t* destination[4]{gray->pixels.data(), nullptr, nullptr, nullptr};
    int stride[4]{width, 0, 0, 0};
    return sws_scale(scaler.get(),
                     frame->data,
                     frame->linesize,
                     0,
                     frame->height,
                     destination,
                     stride) == height;
}

std::string PHashHex(const std::array<uint64_t, 9>& values) {
    std::ostringstream output;
    output << std::hex << std::setfill('0');
    for (size_t i = 0; i < values.size(); ++i) {
        if (i != 0u) output << ',';
        output << std::setw(16) << values[i];
    }
    return output.str();
}

std::string SobelHex(const std::array<float, 128>& values) {
    std::ostringstream output;
    output << std::hex << std::setfill('0');
    for (size_t i = 0; i < values.size(); ++i) {
        uint32_t bits = 0;
        std::memcpy(&bits, &values[i], sizeof(bits));
        if (i != 0u) output << ',';
        output << std::setw(8) << bits;
    }
    return output.str();
}

}  // namespace

int main(int argc, char** argv) {
    if (argc != 2) return 2;
    AVFormatContext* raw_format = nullptr;
    if (avformat_open_input(&raw_format, argv[1], nullptr, nullptr) < 0) return 3;
    AvOwner<AVFormatContext, FreeFormat> format{raw_format};
    if (avformat_find_stream_info(format.value, nullptr) < 0) return 4;
    const AVCodec* decoder = nullptr;
    const int stream_index = av_find_best_stream(
        format.value, AVMEDIA_TYPE_VIDEO, -1, -1, &decoder, 0);
    if (stream_index < 0 || decoder == nullptr) return 5;
    AVStream* stream = format.value->streams[stream_index];
    AvOwner<AVCodecContext, avcodec_free_context> codec{
        avcodec_alloc_context3(decoder)};
    if (codec.value == nullptr ||
        avcodec_parameters_to_context(codec.value, stream->codecpar) < 0) {
        return 6;
    }
    codec.value->thread_count = 1;
    codec.value->thread_type = 0;
    if (avcodec_open2(codec.value, decoder, nullptr) < 0) return 7;
    AvOwner<AVPacket, av_packet_free> packet{av_packet_alloc()};
    AvOwner<AVFrame, av_frame_free> frame{av_frame_alloc()};
    if (packet.value == nullptr || frame.value == nullptr) return 8;

    std::cout << "SAR_PROVENANCE|ffmpeg=" << av_version_info()
              << ";avformat=" << avformat_version()
              << ";avcodec=" << avcodec_version()
              << ";avutil=" << avutil_version()
              << ";swscale=" << swscale_version()
              << ";fixture_sha256=" << Sha256File(argv[1]) << '\n';
    std::cout << "SAR_PARAMS|scale=max_side:512;filter=bicubic;pix_fmt=gray8;"
                 "sar=display-before-scale;seek=decode-from-zero\n";

    constexpr std::array<int64_t, 6> requested_ms{
        150, 450, 750, 1050, 1350, 1650};
    for (size_t sample = 0; sample < requested_ms.size(); ++sample) {
        if (av_seek_frame(format.value, stream_index, 0,
                          AVSEEK_FLAG_BACKWARD) < 0) return 9;
        avcodec_flush_buffers(codec.value);
        int32_t ordinal = -1;
        const int64_t target = av_rescale_q(
            requested_ms[sample], AVRational{1, 1000}, stream->time_base);
        const int64_t start = stream->start_time == AV_NOPTS_VALUE
                                  ? 0
                                  : stream->start_time;
        bool complete = false;
        while (!complete) {
            const int read_status = av_read_frame(format.value, packet.value);
            const bool flushing = read_status == AVERROR_EOF;
            if (read_status < 0 && !flushing) return 10;
            if (!flushing && packet.value->stream_index != stream_index) {
                av_packet_unref(packet.value);
                continue;
            }
            const int send_status = avcodec_send_packet(
                codec.value, flushing ? nullptr : packet.value);
            if (!flushing) av_packet_unref(packet.value);
            if (send_status < 0) return 11;
            for (;;) {
                const int receive_status =
                    avcodec_receive_frame(codec.value, frame.value);
                if (receive_status == AVERROR(EAGAIN)) break;
                if (receive_status == AVERROR_EOF) return 10;
                if (receive_status < 0) return 12;
                ++ordinal;
                const int64_t pts = frame.value->best_effort_timestamp - start;
                if (pts < target) {
                    av_frame_unref(frame.value);
                    continue;
                }
                GrayImage gray;
                if (!ScaleSarGray(format.value, stream, frame.value, &gray)) {
                    return 13;
                }
                std::array<uint8_t, 32> pdq{};
                int32_t quality = 0;
                std::array<uint64_t, 9> phash{};
                std::array<float, 128> sobel{};
                if (videocore::native::ComputePdq(gray, &pdq, &quality) !=
                        ImageStatus::ok ||
                    videocore::native::ComputePHashParts(gray, &phash) !=
                        ImageStatus::ok ||
                    videocore::native::ComputeSobelHistogram(gray, &sobel) !=
                        ImageStatus::ok) {
                    return 14;
                }
                const int64_t pts_us = av_rescale_q(
                    pts, stream->time_base, AVRational{1, 1000000});
                std::cout << "SAR_REFERENCE|" << sample
                          << '|' << requested_ms[sample] * 1000
                          << '|' << ordinal
                          << '|' << pts
                          << '|' << pts_us
                          << '|' << ((frame.value->flags & AV_FRAME_FLAG_KEY) ? 1 : 0)
                          << '|' << av_get_picture_type_char(frame.value->pict_type)
                          << '|' << gray.width
                          << '|' << gray.height
                          << '|' << HexBytes(pdq.data(), pdq.size())
                          << '|' << quality
                          << '|' << PHashHex(phash)
                          << '|' << SobelHex(sobel) << '\n';
                av_frame_unref(frame.value);
                complete = true;
                break;
            }
        }
    }
    return 0;
}
