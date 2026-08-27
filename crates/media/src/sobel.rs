//! 128×128、4×4 空间格和八个无符号方向 bin 的 Sobel 结构特征。

use crate::{GrayImage, resize_bilinear};

const WORK_SIZE: usize = 128;
const GRID_SIZE: usize = 4;
const BIN_COUNT: usize = 8;
const CELL_SIZE: usize = WORK_SIZE / GRID_SIZE;

/// 计算 128 维 L2 归一化 Sobel 结构直方图。
pub fn compute_sobel(image: &GrayImage) -> [f32; GRID_SIZE * GRID_SIZE * BIN_COUNT] {
    let resized = resize_bilinear(image, WORK_SIZE as u32, WORK_SIZE as u32)
        .expect("固定非零目标尺寸必定有效");
    let pixels = resized.pixels();
    let mut histogram = [0.0_f32; GRID_SIZE * GRID_SIZE * BIN_COUNT];

    for y in 1..WORK_SIZE - 1 {
        for x in 1..WORK_SIZE - 1 {
            let top_left = sample(pixels, x - 1, y - 1);
            let top_center = sample(pixels, x, y - 1);
            let top_right = sample(pixels, x + 1, y - 1);
            let middle_left = sample(pixels, x - 1, y);
            let middle_right = sample(pixels, x + 1, y);
            let bottom_left = sample(pixels, x - 1, y + 1);
            let bottom_center = sample(pixels, x, y + 1);
            let bottom_right = sample(pixels, x + 1, y + 1);

            let gradient_x = (top_right + 2.0 * middle_right + bottom_right)
                - (top_left + 2.0 * middle_left + bottom_left);
            let gradient_y = (bottom_left + 2.0 * bottom_center + bottom_right)
                - (top_left + 2.0 * top_center + top_right);
            let magnitude = gradient_x.abs() + gradient_y.abs();
            if magnitude < 1e-6 {
                continue;
            }

            let mut orientation = f64::from(gradient_y).atan2(f64::from(gradient_x));
            if orientation < 0.0 {
                orientation += std::f64::consts::PI;
            }
            if orientation >= std::f64::consts::PI {
                orientation -= std::f64::consts::PI;
            }
            let bin = ((orientation / std::f64::consts::PI * BIN_COUNT as f64) as usize)
                .min(BIN_COUNT - 1);
            let cell_y = y / CELL_SIZE;
            let cell_x = x / CELL_SIZE;
            histogram[(cell_y * GRID_SIZE + cell_x) * BIN_COUNT + bin] += magnitude;
        }
    }

    let squared_norm: f64 = histogram
        .iter()
        .map(|value| f64::from(*value) * f64::from(*value))
        .sum();
    let norm = squared_norm.sqrt();
    if norm > 1e-9 {
        for value in &mut histogram {
            *value = (f64::from(*value) / norm) as f32;
        }
    }
    histogram
}

/// 比较两个 Sobel 向量；双方全零为 1，仅一方全零为 0。
pub fn sobel_cosine(left: &[f32; 128], right: &[f32; 128]) -> f32 {
    let left_norm: f64 = left
        .iter()
        .map(|value| f64::from(*value) * f64::from(*value))
        .sum();
    let right_norm: f64 = right
        .iter()
        .map(|value| f64::from(*value) * f64::from(*value))
        .sum();
    let left_zero = left_norm.sqrt() <= 1e-9;
    let right_zero = right_norm.sqrt() <= 1e-9;
    if left_zero || right_zero {
        return if left_zero && right_zero { 1.0 } else { 0.0 };
    }

    let dot: f64 = left
        .iter()
        .zip(right)
        .map(|(left, right)| f64::from(*left) * f64::from(*right))
        .sum();
    (dot / (left_norm * right_norm).sqrt()) as f32
}

fn sample(pixels: &[u8], x: usize, y: usize) -> f32 {
    pixels[y * WORK_SIZE + x] as f32
}
