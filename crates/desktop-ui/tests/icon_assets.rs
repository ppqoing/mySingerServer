//! Image 2 图标资产必须满足固定画布、可见几何和纯黑 Alpha 契约。

use std::path::{Path, PathBuf};

use image::RgbaImage;

const NAVIGATION: [&str; 11] = [
    "app.png",
    "menu.png",
    "overview.png",
    "nodes.png",
    "scan.png",
    "tasks.png",
    "duplicates.png",
    "review-delete.png",
    "settings.png",
    "index.png",
    "sync.png",
];

const INLINE: [&str; 16] = [
    "search.png",
    "refresh.png",
    "add.png",
    "edit.png",
    "remove.png",
    "connect.png",
    "browse.png",
    "info.png",
    "cancel.png",
    "filter.png",
    "preview.png",
    "retry.png",
    "keep.png",
    "delete.png",
    "save.png",
    "folder.png",
];

#[derive(Debug)]
struct AlphaGeometry {
    bbox_width: u32,
    bbox_height: u32,
    centroid_x: f64,
    centroid_y: f64,
    alpha_total: u64,
}

fn icons_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("ui")
        .join("assets")
        .join("icons")
}

fn load_rgba(name: &str) -> RgbaImage {
    let path = icons_dir().join(name);
    image::open(&path)
        .unwrap_or_else(|error| panic!("应能解码 {}: {error}", path.display()))
        .into_rgba8()
}

fn alpha_geometry(name: &str, image: &RgbaImage) -> AlphaGeometry {
    let mut min_x = image.width();
    let mut min_y = image.height();
    let mut max_x = 0;
    let mut max_y = 0;
    let mut alpha_total = 0_u64;
    let mut weighted_x = 0_f64;
    let mut weighted_y = 0_f64;

    for (x, y, pixel) in image.enumerate_pixels() {
        let alpha = pixel.0[3];
        if alpha == 0 {
            continue;
        }
        assert_eq!(&pixel.0[..3], &[0, 0, 0], "{name} 的非透明像素必须是纯黑");
        min_x = min_x.min(x);
        min_y = min_y.min(y);
        max_x = max_x.max(x);
        max_y = max_y.max(y);
        alpha_total += u64::from(alpha);
        weighted_x += f64::from(x) * f64::from(alpha);
        weighted_y += f64::from(y) * f64::from(alpha);
    }

    assert!(alpha_total > 0, "{name} 必须包含可见像素");
    assert!(
        min_x >= 1 && min_y >= 1 && max_x + 1 < image.width() && max_y + 1 < image.height(),
        "{name} 四边必须各保留至少 1px 透明边距，实际 bbox=({min_x},{min_y})-({max_x},{max_y})"
    );

    AlphaGeometry {
        bbox_width: max_x - min_x + 1,
        bbox_height: max_y - min_y + 1,
        centroid_x: weighted_x / alpha_total as f64,
        centroid_y: weighted_y / alpha_total as f64,
        alpha_total,
    }
}

fn assert_group(names: &[&str], canvas: u32, longest_min: u32, longest_max: u32) {
    let center = (f64::from(canvas) - 1.0) / 2.0;

    for name in names {
        let image = load_rgba(name);
        assert_eq!(
            image.dimensions(),
            (canvas, canvas),
            "{name} 必须使用 {canvas}x{canvas} 画布"
        );
        let geometry = alpha_geometry(name, &image);
        let longest = geometry.bbox_width.max(geometry.bbox_height);
        let shortest = geometry.bbox_width.min(geometry.bbox_height);
        assert!(
            (longest_min..=longest_max).contains(&longest),
            "{name} Alpha bbox 最长边应为 {longest_min}-{longest_max}px，实际 {}x{}px",
            geometry.bbox_width,
            geometry.bbox_height
        );
        assert!(
            shortest * 2 >= longest,
            "{name} Alpha bbox 短边不得小于最长边的一半，实际 {}x{}px",
            geometry.bbox_width,
            geometry.bbox_height
        );
        assert!(
            (geometry.centroid_x - center).abs() <= 0.5
                && (geometry.centroid_y - center).abs() <= 0.5,
            "{name} Alpha 质心必须贴近画布中心 ({center},{center})，实际 ({:.3},{:.3})",
            geometry.centroid_x,
            geometry.centroid_y
        );
        let occupancy =
            geometry.alpha_total as f64 / (f64::from(canvas) * f64::from(canvas) * 255.0);
        assert!(
            (0.02..=0.60).contains(&occupancy),
            "{name} Alpha 占用率应在 2%-60%，实际 {:.2}%",
            occupancy * 100.0
        );
    }
}

#[test]
fn semantic_icons_obey_canvas_alpha_and_black_pixel_contracts() {
    assert_group(&NAVIGATION, 20, 17, 18);
    assert_group(&INLINE, 16, 12, 14);
}

#[test]
fn brand_variants_and_ico_decode_at_declared_sizes() {
    for size in [16_u32, 24, 32, 48, 256] {
        let name = format!("app-{size}.png");
        let image = load_rgba(&name);
        assert_eq!(
            image.dimensions(),
            (size, size),
            "{name} 尺寸必须匹配文件名"
        );
        let geometry = alpha_geometry(&name, &image);
        let center = (f64::from(size) - 1.0) / 2.0;
        assert!(
            (geometry.centroid_x - center).abs() <= 0.5
                && (geometry.centroid_y - center).abs() <= 0.5,
            "{name} Alpha 质心必须贴近画布中心 ({center},{center})，实际 ({:.3},{:.3})",
            geometry.centroid_x,
            geometry.centroid_y
        );
    }

    let ico = icons_dir().join("app.ico");
    image::open(&ico)
        .unwrap_or_else(|error| panic!("应能用 ICO decoder 打开 {}: {error}", ico.display()));
}
