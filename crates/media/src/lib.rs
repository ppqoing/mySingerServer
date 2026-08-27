//! MD5、PDQ、分块 pHash、Sobel、视频评分和联系表算法。
#![warn(missing_docs)]

mod contact_sheet;
mod image;
mod image_score;
mod pdq;
mod phash;
mod resize;
mod sobel;
mod video_score;

pub use contact_sheet::{
    ContactSheetError, decode_contact_sheet, decode_contact_sheet_slots, encode_contact_sheet,
};
pub use image::{GrayImage, MediaError, Rgb24Image, rgb24_to_gray};
pub use image_score::{
    ImageStage1, ImageStage1Score, ImageStage2, ImageStage2Score, compute_image_stage2, pdq_bands,
    screen_image_stage1, screen_image_stage2, shares_pdq_band,
};
pub use pdq::{PdqHash, PdqResult, pdq_hash};
pub use phash::{compute_partition_phash, phash_parts_to_blob};
pub use resize::resize_bilinear;
pub use sobel::{compute_sobel, sobel_cosine};
pub use video_score::{
    VideoFrameFeatures, VideoScore, sample_positions, score_video_stage1, score_video_stage2,
};

#[cfg(test)]
mod tests {
    use super::{
        GrayImage, PdqHash, PdqResult, Rgb24Image, pdq_hash, resize_bilinear, rgb24_to_gray,
    };

    /// 防止灰度转换改用浮点或不同权重后让全部特征字节漂移。
    #[test]
    fn rgb24_uses_confirmed_integer_luma() {
        let rgb = Rgb24Image::new(1, 1, vec![255, 0, 0]).unwrap();
        assert_eq!(rgb24_to_gray(&rgb).pixels(), &[77]);
    }

    /// 防止缩放改用角点对齐，破坏 PDQ、pHash 和 Sobel 的共同输入。
    #[test]
    fn bilinear_resize_uses_pixel_centers() {
        let source = GrayImage::new(2, 1, vec![0, 100]).unwrap();
        assert_eq!(
            resize_bilinear(&source, 4, 1).unwrap().pixels(),
            &[0, 25, 75, 100]
        );
    }

    /// 读取固定 JPEG 时必须先得到 RGB24，再走项目唯一的整数灰度管线。
    fn pdq_fixture(bytes: &[u8]) -> PdqResult {
        let decoded = image::load_from_memory_with_format(bytes, image::ImageFormat::Jpeg)
            .unwrap()
            .to_rgb8();
        let (width, height) = decoded.dimensions();
        let rgb = Rgb24Image::new(width, height, decoded.into_raw()).unwrap();
        pdq_hash(&rgb24_to_gray(&rgb))
    }

    /// 锁定 Meta 原始桥图的 256 位 PDQ 位序与 Quality。
    #[test]
    fn pdq_bridge_original_matches_meta_golden() {
        let result = pdq_fixture(include_bytes!("../testdata/pdq/bridge-original.jpg"));
        assert_eq!(
            result.hash.to_hex(),
            "f8f8f0cee0f4a84f06370a22038f63f0b36e2ed596621e1d33e6b39c4e9c9b22"
        );
        assert_eq!(result.quality, 100);
    }

    /// 锁定轻微模糊图仍与上游 golden 逐位一致。
    #[test]
    fn pdq_blur_a_little_matches_meta_golden() {
        let result = pdq_fixture(include_bytes!("../testdata/pdq/blur-a-little.jpg"));
        assert_eq!(
            result.hash.to_hex(),
            "f8f8f0cee0f4a84f06370a2a038f63f0b36e26d596621e1d33e6b39c4e9c9b22"
        );
        assert_eq!(result.quality, 100);
    }

    /// 锁定过小输入返回上游定义的零质量结果而不是拒绝文件。
    #[test]
    fn pdq_small_image_matches_meta_golden() {
        let result = pdq_fixture(include_bytes!("../testdata/pdq/small.jpg"));
        assert_eq!(
            result.hash.to_hex(),
            "0007001f003f003f007f00ff00ff00ff01ff01ff01ff03ff03ff03ff03ff03ff"
        );
        assert_eq!(result.quality, 0);
    }

    /// 汉明距离直接使用规范网络字节数组，避免再次解释上游字序。
    #[test]
    fn pdq_hamming_distance_counts_bits() {
        let zero = PdqHash::from_bytes([0; 32]);
        let mut one_bit = [0; 32];
        one_bit[17] = 0b0000_0100;
        assert_eq!(zero.hamming_distance(&zero), 0);
        assert_eq!(zero.hamming_distance(&PdqHash::from_bytes(one_bit)), 1);
    }
}
