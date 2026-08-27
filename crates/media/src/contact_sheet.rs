//! 复用六个视频槽位生成三列两行、RGB24、质量 80 的 JPG 联系表。

use image::{ExtendedColorType, ImageError, codecs::jpeg::JpegEncoder};
use thiserror::Error;

use crate::{GrayImage, MediaError, Rgb24Image, rgb24_to_gray};

const COLUMNS: u32 = 3;
const ROWS: u32 = 2;
const MISSING_COLOR: [u8; 3] = [0x60, 0x65, 0x6f];

/// 解码固定三列两行联系表时的格式或槽位错误。
#[derive(Debug, Error)]
pub enum ContactSheetError {
    /// JPEG 内容损坏或不是可解码图片。
    #[error("联系表 JPEG 解码失败: {0}")]
    Decode(#[from] ImageError),
    /// 联系表尺寸必须能等分为三列两行。
    #[error("联系表尺寸无效: {width}x{height}")]
    InvalidDimensions {
        /// JPEG 像素宽度。
        width: u32,
        /// JPEG 像素高度。
        height: u32,
    },
    /// 视频槽位必须位于固定六槽范围。
    #[error("联系表槽位无效: {0}")]
    InvalidSlot(u8),
    /// 裁剪后的 RGB24 长度与尺寸不一致。
    #[error(transparent)]
    InvalidPixels(#[from] MediaError),
}

/// 把六个已有槽位按行优先编码为 JPG；`None` 使用固定灰色单元格。
pub fn encode_contact_sheet(
    frames: &[Option<Rgb24Image>; 6],
    cell_width: u32,
    cell_height: u32,
) -> Result<Vec<u8>, ImageError> {
    let canvas_width = cell_width * COLUMNS;
    let canvas_height = cell_height * ROWS;
    let mut canvas = MISSING_COLOR.repeat((canvas_width * canvas_height) as usize);

    for (slot, frame) in frames.iter().enumerate() {
        let Some(frame) = frame else {
            continue;
        };
        let (target_width, target_height) = fitted_size(frame, cell_width, cell_height);
        let resized = resize_rgb24(frame, target_width, target_height);
        let cell_x = slot as u32 % COLUMNS * cell_width;
        let cell_y = slot as u32 / COLUMNS * cell_height;
        let offset_x = cell_x + (cell_width - target_width) / 2;
        let offset_y = cell_y + (cell_height - target_height) / 2;
        blit_rgb24(
            &mut canvas,
            canvas_width,
            &resized,
            target_width,
            target_height,
            offset_x,
            offset_y,
        );
    }

    let mut jpeg = Vec::new();
    JpegEncoder::new_with_quality(&mut jpeg, 80).encode(
        &canvas,
        canvas_width,
        canvas_height,
        ExtendedColorType::Rgb8,
    )?;
    Ok(jpeg)
}

/// 一次解码联系表 JPEG，并按请求顺序裁剪固定三列两行中的槽位。
pub fn decode_contact_sheet_slots(
    jpeg: &[u8],
    slots: &[u8],
) -> Result<Vec<(u8, Rgb24Image)>, ContactSheetError> {
    let decoded = image::load_from_memory_with_format(jpeg, image::ImageFormat::Jpeg)?.to_rgb8();
    let (width, height) = decoded.dimensions();
    if width == 0 || height == 0 || width % COLUMNS != 0 || height % ROWS != 0 {
        return Err(ContactSheetError::InvalidDimensions { width, height });
    }
    let cell_width = width / COLUMNS;
    let cell_height = height / ROWS;
    let raw = decoded.as_raw();
    let mut output = Vec::with_capacity(slots.len());
    for &slot in slots {
        if usize::from(slot) >= 6 {
            return Err(ContactSheetError::InvalidSlot(slot));
        }
        let offset_x = u32::from(slot) % COLUMNS * cell_width;
        let offset_y = u32::from(slot) / COLUMNS * cell_height;
        let mut pixels = Vec::with_capacity((cell_width * cell_height * 3) as usize);
        for y in 0..cell_height {
            let start = (((offset_y + y) * width + offset_x) * 3) as usize;
            let bytes = (cell_width * 3) as usize;
            pixels.extend_from_slice(&raw[start..start + bytes]);
        }
        output.push((slot, Rgb24Image::new(cell_width, cell_height, pixels)?));
    }
    Ok(output)
}

/// 一次解码完整联系表，并按行优先返回六个灰度单元格。
pub fn decode_contact_sheet(jpeg: &[u8]) -> Result<[GrayImage; 6], ContactSheetError> {
    let slots = [0, 1, 2, 3, 4, 5];
    let frames = decode_contact_sheet_slots(jpeg, &slots)?
        .into_iter()
        .map(|(_, rgb)| rgb24_to_gray(&rgb))
        .collect::<Vec<_>>();
    frames
        .try_into()
        .map_err(|_| ContactSheetError::InvalidDimensions {
            width: 0,
            height: 0,
        })
}

fn fitted_size(frame: &Rgb24Image, cell_width: u32, cell_height: u32) -> (u32, u32) {
    let scale =
        (cell_width as f64 / frame.width() as f64).min(cell_height as f64 / frame.height() as f64);
    let width = (frame.width() as f64 * scale)
        .round()
        .clamp(1.0, cell_width as f64) as u32;
    let height = (frame.height() as f64 * scale)
        .round()
        .clamp(1.0, cell_height as f64) as u32;
    (width, height)
}

/// 使用与灰度缩放相同的像素中心坐标与边缘钳制规则处理三个通道。
fn resize_rgb24(frame: &Rgb24Image, target_width: u32, target_height: u32) -> Vec<u8> {
    let mut output = Vec::with_capacity((target_width * target_height * 3) as usize);
    for target_y in 0..target_height {
        let source_y = source_coordinate(target_y, frame.height(), target_height);
        let y0 = source_y.floor() as u32;
        let y1 = (y0 + 1).min(frame.height() - 1);
        let fy = source_y - y0 as f64;
        for target_x in 0..target_width {
            let source_x = source_coordinate(target_x, frame.width(), target_width);
            let x0 = source_x.floor() as u32;
            let x1 = (x0 + 1).min(frame.width() - 1);
            let fx = source_x - x0 as f64;
            for channel in 0..3 {
                let top = lerp(
                    sample(frame, x0, y0, channel),
                    sample(frame, x1, y0, channel),
                    fx,
                );
                let bottom = lerp(
                    sample(frame, x0, y1, channel),
                    sample(frame, x1, y1, channel),
                    fx,
                );
                output.push(lerp(top, bottom, fy).round().clamp(0.0, 255.0) as u8);
            }
        }
    }
    output
}

fn source_coordinate(target: u32, source_size: u32, target_size: u32) -> f64 {
    ((target as f64 + 0.5) * source_size as f64 / target_size as f64 - 0.5)
        .clamp(0.0, (source_size - 1) as f64)
}

fn sample(frame: &Rgb24Image, x: u32, y: u32, channel: usize) -> f64 {
    frame.pixels()[((y * frame.width() + x) * 3) as usize + channel] as f64
}

fn lerp(left: f64, right: f64, amount: f64) -> f64 {
    left + (right - left) * amount
}

fn blit_rgb24(
    canvas: &mut [u8],
    canvas_width: u32,
    pixels: &[u8],
    width: u32,
    height: u32,
    offset_x: u32,
    offset_y: u32,
) {
    for y in 0..height {
        let source_start = (y * width * 3) as usize;
        let target_start = (((offset_y + y) * canvas_width + offset_x) * 3) as usize;
        let bytes = (width * 3) as usize;
        canvas[target_start..target_start + bytes]
            .copy_from_slice(&pixels[source_start..source_start + bytes]);
    }
}
