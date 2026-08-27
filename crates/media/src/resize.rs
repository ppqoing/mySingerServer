//! 共享像素中心坐标和边缘钳制规则的灰度双线性缩放。

use crate::{GrayImage, MediaError};

/// 使用像素中心对齐把灰度图缩放到目标宽高。
pub fn resize_bilinear(
    source: &GrayImage,
    target_width: u32,
    target_height: u32,
) -> Result<GrayImage, MediaError> {
    if target_width == 0 || target_height == 0 {
        return Err(MediaError::EmptyDimensions);
    }

    let mut output = Vec::with_capacity(target_width as usize * target_height as usize);
    for target_y in 0..target_height {
        let source_y = source_coordinate(target_y, source.height(), target_height);
        let y0 = source_y.floor().max(0.0) as u32;
        let y1 = (y0 + 1).min(source.height() - 1);
        let fy = source_y.clamp(0.0, (source.height() - 1) as f32) - y0 as f32;

        for target_x in 0..target_width {
            let source_x = source_coordinate(target_x, source.width(), target_width);
            let x0 = source_x.floor().max(0.0) as u32;
            let x1 = (x0 + 1).min(source.width() - 1);
            let fx = source_x.clamp(0.0, (source.width() - 1) as f32) - x0 as f32;

            let top = lerp(sample(source, x0, y0), sample(source, x1, y0), fx);
            let bottom = lerp(sample(source, x0, y1), sample(source, x1, y1), fx);
            output.push(lerp(top, bottom, fy).round().clamp(0.0, 255.0) as u8);
        }
    }
    Ok(GrayImage::from_validated(
        target_width,
        target_height,
        output,
    ))
}

fn source_coordinate(target: u32, source_size: u32, target_size: u32) -> f32 {
    ((target as f32 + 0.5) * source_size as f32 / target_size as f32 - 0.5)
        .clamp(0.0, (source_size - 1) as f32)
}

fn sample(image: &GrayImage, x: u32, y: u32) -> f32 {
    image.pixels()[(y * image.width() + x) as usize] as f32
}

fn lerp(left: f32, right: f32, amount: f32) -> f32 {
    left + (right - left) * amount
}
