//! 96×96、3×3 分块和固定 8×8 DCT 的图片二筛 pHash。

use std::sync::OnceLock;

use crate::{GrayImage, resize_bilinear};

const WORK_SIZE: usize = 96;
const PART_SIZE: usize = 32;
const DCT_SIZE: usize = 8;

/// 把灰度图缩放一次，并按 3×3 行优先计算九个 64 位 pHash。
pub fn compute_partition_phash(image: &GrayImage) -> [u64; 9] {
    let resized = resize_bilinear(image, WORK_SIZE as u32, WORK_SIZE as u32)
        .expect("固定非零目标尺寸必定有效");
    let mut parts = [0_u64; 9];
    for part_row in 0..3 {
        for part_column in 0..3 {
            parts[part_row * 3 + part_column] = hash_part(&resized, part_row, part_column);
        }
    }
    parts
}

/// 按块行优先和 `u64` 小端顺序生成数据库使用的固定 72 字节 BLOB。
pub fn phash_parts_to_blob(parts: &[u64; 9]) -> [u8; 72] {
    let mut blob = [0_u8; 72];
    for (index, part) in parts.iter().enumerate() {
        blob[index * 8..index * 8 + 8].copy_from_slice(&part.to_le_bytes());
    }
    blob
}

fn hash_part(image: &GrayImage, part_row: usize, part_column: usize) -> u64 {
    let table = cosine_table();
    let mut coefficients = [0.0_f32; DCT_SIZE * DCT_SIZE];
    let base_y = part_row * PART_SIZE;
    let base_x = part_column * PART_SIZE;

    for vertical_frequency in 0..DCT_SIZE {
        for horizontal_frequency in 0..DCT_SIZE {
            let mut sum = 0.0_f64;
            for y in 0..PART_SIZE {
                let row_offset = (base_y + y) * WORK_SIZE + base_x;
                let mut row_sum = 0.0_f64;
                for x in 0..PART_SIZE {
                    row_sum += image.pixels()[row_offset + x] as f64
                        * table[x * DCT_SIZE + horizontal_frequency];
                }
                sum += row_sum * table[y * DCT_SIZE + vertical_frequency];
            }
            let horizontal_scale = if horizontal_frequency == 0 {
                std::f64::consts::FRAC_1_SQRT_2
            } else {
                1.0
            };
            let vertical_scale = if vertical_frequency == 0 {
                std::f64::consts::FRAC_1_SQRT_2
            } else {
                1.0
            };
            coefficients[vertical_frequency * DCT_SIZE + horizontal_frequency] =
                (0.25 * horizontal_scale * vertical_scale * sum) as f32;
        }
    }

    let mut ordered = coefficients;
    let (_, median, _) = ordered.select_nth_unstable_by(32, f32::total_cmp);
    coefficients
        .into_iter()
        .enumerate()
        .fold(0_u64, |hash, (index, coefficient)| {
            if coefficient > *median {
                hash | (1_u64 << index)
            } else {
                hash
            }
        })
}

fn cosine_table() -> &'static [f64; PART_SIZE * DCT_SIZE] {
    static TABLE: OnceLock<[f64; PART_SIZE * DCT_SIZE]> = OnceLock::new();
    TABLE.get_or_init(|| {
        let mut values = [0.0; PART_SIZE * DCT_SIZE];
        for position in 0..PART_SIZE {
            for frequency in 0..DCT_SIZE {
                values[position * DCT_SIZE + frequency] =
                    ((2 * position + 1) as f64 * frequency as f64 * std::f64::consts::PI / 64.0)
                        .cos();
            }
        }
        values
    })
}
