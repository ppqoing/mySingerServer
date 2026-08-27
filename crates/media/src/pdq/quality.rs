//! Meta PDQ 对 64×64 图像域相邻梯度的固定 Quality 计算。

use super::downscale::PDQ_SIZE;

/// 计算 `0..=100` 的 PDQ 图像质量；算术顺序与上游 C++ 保持一致。
pub(super) fn image_domain_quality(image: &[f32; PDQ_SIZE * PDQ_SIZE]) -> u8 {
    let mut gradient_sum = 0_i32;

    for row in 0..PDQ_SIZE - 1 {
        for column in 0..PDQ_SIZE {
            gradient_sum += quantized_difference(
                image[row * PDQ_SIZE + column],
                image[(row + 1) * PDQ_SIZE + column],
            );
        }
    }
    for row in 0..PDQ_SIZE {
        for column in 0..PDQ_SIZE - 1 {
            gradient_sum += quantized_difference(
                image[row * PDQ_SIZE + column],
                image[row * PDQ_SIZE + column + 1],
            );
        }
    }

    (gradient_sum / 90).min(100) as u8
}

fn quantized_difference(left: f32, right: f32) -> i32 {
    ((((left - right) * 100.0) / 255.0) as i32).abs()
}
