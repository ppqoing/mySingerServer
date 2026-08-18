//! Meta PDQ 固定的 64×64 到非 DC 16×16 二维 DCT。

use super::downscale::PDQ_SIZE;

const OUTPUT_SIZE: usize = 16;

/// 只计算完整 DCT 的频率槽位 `1..=16`，保持上游逐项 `f32` 累加顺序。
pub(super) fn dct_64_to_16(input: &[f32; PDQ_SIZE * PDQ_SIZE]) -> [f32; OUTPUT_SIZE * OUTPUT_SIZE] {
    let matrix = dct_matrix();
    let mut intermediate = [0.0_f32; OUTPUT_SIZE * PDQ_SIZE];

    for output_row in 0..OUTPUT_SIZE {
        for column in 0..PDQ_SIZE {
            let mut sum = 0.0_f32;
            for row in 0..PDQ_SIZE {
                sum += matrix[output_row * PDQ_SIZE + row] * input[row * PDQ_SIZE + column];
            }
            intermediate[output_row * PDQ_SIZE + column] = sum;
        }
    }

    let mut output = [0.0_f32; OUTPUT_SIZE * OUTPUT_SIZE];
    for row in 0..OUTPUT_SIZE {
        for output_column in 0..OUTPUT_SIZE {
            let mut sum = 0.0_f32;
            for column in 0..PDQ_SIZE {
                sum += intermediate[row * PDQ_SIZE + column]
                    * matrix[output_column * PDQ_SIZE + column];
            }
            output[row * OUTPUT_SIZE + output_column] = sum;
        }
    }
    output
}

fn dct_matrix() -> [f32; OUTPUT_SIZE * PDQ_SIZE] {
    let mut matrix = [0.0_f32; OUTPUT_SIZE * PDQ_SIZE];
    let scale = (2.0_f64 / PDQ_SIZE as f64).sqrt();
    for row in 0..OUTPUT_SIZE {
        for column in 0..PDQ_SIZE {
            let angle = (std::f64::consts::PI / 2.0 / PDQ_SIZE as f64)
                * (row + 1) as f64
                * (2 * column + 1) as f64;
            matrix[row * PDQ_SIZE + column] = (scale * angle.cos()) as f32;
        }
    }
    matrix
}
