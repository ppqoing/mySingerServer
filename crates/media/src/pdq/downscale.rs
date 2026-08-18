//! Meta PDQ 的两轮 Jarosz 方框滤波与 64×64 中心抽样。

use crate::GrayImage;

/// PDQ 固定图像域边长。
pub(super) const PDQ_SIZE: usize = 64;

/// 把已验证灰度图转换为 PDQ 使用的 64×64 `f32` 亮度矩阵。
pub(super) fn downsample_64(image: &GrayImage) -> [f32; PDQ_SIZE * PDQ_SIZE] {
    let rows = image.height() as usize;
    let columns = image.width() as usize;
    let mut buffer1: Vec<f32> = image.pixels().iter().map(|value| *value as f32).collect();

    if rows == PDQ_SIZE && columns == PDQ_SIZE {
        return buffer1
            .try_into()
            .expect("64x64 图像长度已经由 GrayImage 保证");
    }

    let mut buffer2 = vec![0.0; buffer1.len()];
    let row_window = jarosz_window(columns, PDQ_SIZE);
    let column_window = jarosz_window(rows, PDQ_SIZE);

    for _ in 0..2 {
        box_along_rows(&buffer1, &mut buffer2, rows, columns, row_window);
        box_along_columns(&buffer2, &mut buffer1, rows, columns, column_window);
    }

    decimate(&buffer1, rows, columns)
}

fn jarosz_window(old_dimension: usize, new_dimension: usize) -> usize {
    old_dimension.div_ceil(2 * new_dimension)
}

fn box_along_rows(input: &[f32], output: &mut [f32], rows: usize, columns: usize, window: usize) {
    for row in 0..rows {
        box_1d(input, output, row * columns, columns, 1, window);
    }
}

fn box_along_columns(
    input: &[f32],
    output: &mut [f32],
    rows: usize,
    columns: usize,
    window: usize,
) {
    for column in 0..columns {
        box_1d(input, output, column, rows, columns, window);
    }
}

/// 保持上游四阶段累加、写入和相减顺序，避免浮点舍入改变最终 hash 位。
fn box_1d(
    input: &[f32],
    output: &mut [f32],
    start: usize,
    vector_length: usize,
    stride: usize,
    full_window_size: usize,
) {
    let half_window_size = (full_window_size + 2) / 2;
    let phase_1_repetitions = half_window_size - 1;
    let phase_2_repetitions = full_window_size - half_window_size + 1;
    let phase_3_repetitions = vector_length - full_window_size;
    let phase_4_repetitions = half_window_size - 1;

    let mut left = 0;
    let mut right = 0;
    let mut output_index = 0;
    let mut sum = 0.0_f32;
    let mut current_window_size = 0_u32;

    for _ in 0..phase_1_repetitions {
        sum += input[start + right * stride];
        current_window_size += 1;
        right += 1;
    }
    for _ in 0..phase_2_repetitions {
        sum += input[start + right * stride];
        current_window_size += 1;
        output[start + output_index * stride] = sum / current_window_size as f32;
        right += 1;
        output_index += 1;
    }
    for _ in 0..phase_3_repetitions {
        sum += input[start + right * stride];
        sum -= input[start + left * stride];
        output[start + output_index * stride] = sum / current_window_size as f32;
        left += 1;
        right += 1;
        output_index += 1;
    }
    for _ in 0..phase_4_repetitions {
        sum -= input[start + left * stride];
        current_window_size -= 1;
        output[start + output_index * stride] = sum / current_window_size as f32;
        left += 1;
        output_index += 1;
    }
}

fn decimate(input: &[f32], input_rows: usize, input_columns: usize) -> [f32; PDQ_SIZE * PDQ_SIZE] {
    let mut output = [0.0; PDQ_SIZE * PDQ_SIZE];
    for output_row in 0..PDQ_SIZE {
        let input_row =
            (((output_row as f64 + 0.5) * input_rows as f64) / PDQ_SIZE as f64) as usize;
        for output_column in 0..PDQ_SIZE {
            let input_column =
                (((output_column as f64 + 0.5) * input_columns as f64) / PDQ_SIZE as f64) as usize;
            output[output_row * PDQ_SIZE + output_column] =
                input[input_row * input_columns + input_column];
        }
    }
    output
}
