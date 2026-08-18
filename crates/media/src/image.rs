//! 已在构造边界验证尺寸的拥有所有权 RGB24 与灰度像素缓冲区。

use thiserror::Error;

/// 像素缓冲区尺寸与声明的宽、高或通道数不一致。
#[derive(Debug, Error)]
pub enum MediaError {
    /// 宽、高或通道乘积溢出或不等于实际像素长度。
    #[error("像素缓冲区长度无效: {width}x{height}x{channels}，实际 {actual}")]
    InvalidPixelLength {
        /// 声明宽度。
        width: u32,
        /// 声明高度。
        height: u32,
        /// 固定通道数。
        channels: u8,
        /// 实际缓冲区字节数。
        actual: usize,
    },
    /// 缩放目标宽或高为零。
    #[error("图像宽高必须大于零")]
    EmptyDimensions,
}

/// 紧凑行优先、每像素三个字节的 RGB24 图像。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Rgb24Image {
    width: u32,
    height: u32,
    pixels: Vec<u8>,
}

impl Rgb24Image {
    /// 创建并一次性验证 `width * height * 3 == pixels.len()`。
    pub fn new(width: u32, height: u32, pixels: Vec<u8>) -> Result<Self, MediaError> {
        validate_length(width, height, 3, pixels.len())?;
        Ok(Self {
            width,
            height,
            pixels,
        })
    }

    /// 返回像素宽度。
    pub const fn width(&self) -> u32 {
        self.width
    }

    /// 返回像素高度。
    pub const fn height(&self) -> u32 {
        self.height
    }

    /// 返回紧凑 RGB24 字节。
    pub fn pixels(&self) -> &[u8] {
        &self.pixels
    }
}

/// 紧凑行优先、每像素一个字节的灰度图像。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GrayImage {
    width: u32,
    height: u32,
    pixels: Vec<u8>,
}

impl GrayImage {
    /// 创建并一次性验证 `width * height == pixels.len()`。
    pub fn new(width: u32, height: u32, pixels: Vec<u8>) -> Result<Self, MediaError> {
        validate_length(width, height, 1, pixels.len())?;
        Ok(Self {
            width,
            height,
            pixels,
        })
    }

    /// 返回像素宽度。
    pub const fn width(&self) -> u32 {
        self.width
    }

    /// 返回像素高度。
    pub const fn height(&self) -> u32 {
        self.height
    }

    /// 返回紧凑灰度字节。
    pub fn pixels(&self) -> &[u8] {
        &self.pixels
    }

    pub(crate) fn from_validated(width: u32, height: u32, pixels: Vec<u8>) -> Self {
        Self {
            width,
            height,
            pixels,
        }
    }
}

/// 使用固定整数亮度公式把已验证 RGB24 图像转换为灰度图。
pub fn rgb24_to_gray(image: &Rgb24Image) -> GrayImage {
    let pixels = image
        .pixels
        .chunks_exact(3)
        .map(|rgb| {
            let luminance =
                77 * u32::from(rgb[0]) + 150 * u32::from(rgb[1]) + 29 * u32::from(rgb[2]) + 128;
            (luminance >> 8) as u8
        })
        .collect();
    GrayImage::from_validated(image.width, image.height, pixels)
}

fn validate_length(width: u32, height: u32, channels: u8, actual: usize) -> Result<(), MediaError> {
    let expected = (width as usize)
        .checked_mul(height as usize)
        .and_then(|pixels| pixels.checked_mul(channels as usize));
    if width == 0 || height == 0 || expected != Some(actual) {
        return Err(MediaError::InvalidPixelLength {
            width,
            height,
            channels,
            actual,
        });
    }
    Ok(())
}
