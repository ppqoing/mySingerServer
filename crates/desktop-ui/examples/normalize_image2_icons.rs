//! 把内置 Image 2 的透明原图确定性归一为桌面 UI 使用的小尺寸纯黑图标。

use std::env;
use std::error::Error;
use std::fs::{self, File};
use std::path::{Path, PathBuf};

use image::codecs::ico::IcoEncoder;
use image::imageops::{crop_imm, overlay, resize, FilterType};
use image::{ExtendedColorType, ImageEncoder, Rgba, RgbaImage};

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

#[derive(Clone, Copy, Debug)]
struct Bounds {
    min_x: u32,
    min_y: u32,
    max_x: u32,
    max_y: u32,
}

#[derive(Clone, Copy, Debug)]
struct Geometry {
    bounds: Bounds,
    centroid_x: f64,
    centroid_y: f64,
    alpha_total: u64,
}

#[derive(Clone, Copy, Debug)]
struct RawTopology {
    width: u32,
    height: u32,
    significant_components: usize,
    dominant_component_ratio: f64,
    subject_span_ratio: f64,
}

const RAW_ALPHA_THRESHOLD: u8 = 32;
const MAX_SIGNIFICANT_COMPONENTS: usize = 12;
const MIN_DOMINANT_COMPONENT_RATIO: f64 = 0.20;

fn main() -> Result<(), Box<dyn Error>> {
    let (input, output) = parse_arguments()?;
    fs::create_dir_all(&output)?;

    let navigation = normalize_group(&input, &NAVIGATION, 20, 18, 17)?;
    let inline = normalize_group(&input, &INLINE, 16, 14, 12)?;

    for (name, image) in navigation.iter().chain(inline.iter()) {
        let topology = icon_topology(image, 1)?;
        println!(
            "final-stats {name}: {}x{}, components={}, dominant={:.1}%, subject-span={:.1}%",
            topology.width,
            topology.height,
            topology.significant_components,
            topology.dominant_component_ratio * 100.0,
            topology.subject_span_ratio * 100.0
        );
        image.save(output.join(name))?;
    }

    let app_source = load_transparent_source(&input.join("app.png"))?;
    for size in [16_u32, 24, 32, 48, 256] {
        let image = normalize_brand(&app_source, size)?;
        image.save(output.join(format!("app-{size}.png")))?;
    }
    encode_ico(&output.join("app-256.png"), &output.join("app.ico"))?;

    println!(
        "已归一化 {} 枚语义图标、5 枚品牌 PNG 和 app.ico 到 {}",
        NAVIGATION.len() + INLINE.len(),
        output.display()
    );
    Ok(())
}

fn parse_arguments() -> Result<(PathBuf, PathBuf), Box<dyn Error>> {
    let mut input = None;
    let mut output = None;
    let mut arguments = env::args_os().skip(1);
    while let Some(argument) = arguments.next() {
        match argument.to_str() {
            Some("--input") => input = arguments.next().map(PathBuf::from),
            Some("--output") => output = arguments.next().map(PathBuf::from),
            _ => return Err(format!("未知参数：{}", argument.to_string_lossy()).into()),
        }
    }
    let input = input.ok_or("缺少 --input <目录>")?;
    let output = output.ok_or("缺少 --output <目录>")?;
    Ok((input, output))
}

fn normalize_group(
    input: &Path,
    names: &[&str],
    canvas: u32,
    target_longest: u32,
    minimum_longest: u32,
) -> Result<Vec<(String, RgbaImage)>, Box<dyn Error>> {
    names
        .iter()
        .map(|name| {
            let source = load_transparent_source(&input.join(name))?;
            let image = normalize_icon(name, &source, canvas, target_longest, minimum_longest)?;
            Ok(((*name).to_owned(), image))
        })
        .collect()
}

fn load_transparent_source(path: &Path) -> Result<RgbaImage, Box<dyn Error>> {
    let image = image::open(path)
        .map_err(|error| format!("无法解码 Image 2 原图 {}：{error}", path.display()))?
        .into_rgba8();
    let width = image.width();
    let height = image.height();
    if width == 0 || height == 0 {
        return Err(format!("Image 2 原图 {} 画布为空", path.display()).into());
    }

    let opaque_border = (0..width).any(|x| image.get_pixel(x, 0).0[3] != 0)
        || (0..width).any(|x| image.get_pixel(x, height - 1).0[3] != 0)
        || (0..height).any(|y| image.get_pixel(0, y).0[3] != 0)
        || (0..height).any(|y| image.get_pixel(width - 1, y).0[3] != 0);
    if opaque_border {
        return Err(format!("Image 2 原图 {} 的背景边界不是完全透明", path.display()).into());
    }
    geometry(&image)
        .map_err(|error| -> Box<dyn Error> { format!("{}：{error}", path.display()).into() })?;
    let topology = raw_topology(&image)?;
    println!(
        "raw-stats {}: {}x{}, significant-components={}, dominant={:.1}%, subject-span={:.1}%",
        path.file_name().map_or_else(
            || path.display().to_string(),
            |name| name.to_string_lossy().into_owned()
        ),
        topology.width,
        topology.height,
        topology.significant_components,
        topology.dominant_component_ratio * 100.0,
        topology.subject_span_ratio * 100.0
    );
    validate_raw_topology(path, topology)?;
    Ok(image)
}

fn validate_raw_topology(path: &Path, topology: RawTopology) -> Result<(), Box<dyn Error>> {
    if topology.significant_components <= MAX_SIGNIFICANT_COMPONENTS
        && topology.dominant_component_ratio >= MIN_DOMINANT_COMPONENT_RATIO
    {
        return Ok(());
    }

    Err(format!(
        "Image 2 原图 {} 疑似候选表：显著组件 {}（最多 {}），最大组件占比 {:.1}%（至少 {:.1}%），主体跨度 {:.1}%",
        path.display(),
        topology.significant_components,
        MAX_SIGNIFICANT_COMPONENTS,
        topology.dominant_component_ratio * 100.0,
        MIN_DOMINANT_COMPONENT_RATIO * 100.0,
        topology.subject_span_ratio * 100.0
    )
    .into())
}

fn raw_topology(image: &RgbaImage) -> Result<RawTopology, Box<dyn Error>> {
    icon_topology(image, 64)
}

fn icon_topology(
    image: &RgbaImage,
    minimum_component_pixels: usize,
) -> Result<RawTopology, Box<dyn Error>> {
    let width = image.width();
    let height = image.height();
    let pixel_count = usize::try_from(u64::from(width) * u64::from(height))?;
    let mut foreground = vec![false; pixel_count];
    let mut foreground_count = 0_usize;
    let mut min_x = width;
    let mut min_y = height;
    let mut max_x = 0_u32;
    let mut max_y = 0_u32;
    for (x, y, pixel) in image.enumerate_pixels() {
        if pixel.0[3] < RAW_ALPHA_THRESHOLD {
            continue;
        }
        let index = usize::try_from(u64::from(y) * u64::from(width) + u64::from(x))?;
        foreground[index] = true;
        foreground_count += 1;
        min_x = min_x.min(x);
        min_y = min_y.min(y);
        max_x = max_x.max(x);
        max_y = max_y.max(y);
    }
    if foreground_count == 0 {
        return Err(format!("图像在 Alpha>={RAW_ALPHA_THRESHOLD} 时没有可见像素").into());
    }

    let mut visited = vec![false; pixel_count];
    let mut component_sizes = Vec::new();
    for start in 0..pixel_count {
        if !foreground[start] || visited[start] {
            continue;
        }
        visited[start] = true;
        let mut stack = vec![start];
        let mut size = 0_usize;
        while let Some(index) = stack.pop() {
            size += 1;
            let x = index % usize::try_from(width)?;
            let y = index / usize::try_from(width)?;
            for offset_y in -1_i32..=1 {
                for offset_x in -1_i32..=1 {
                    if offset_x == 0 && offset_y == 0 {
                        continue;
                    }
                    let neighbor_x = i32::try_from(x)? + offset_x;
                    let neighbor_y = i32::try_from(y)? + offset_y;
                    if neighbor_x < 0
                        || neighbor_y < 0
                        || neighbor_x >= i32::try_from(width)?
                        || neighbor_y >= i32::try_from(height)?
                    {
                        continue;
                    }
                    let neighbor = usize::try_from(neighbor_y)? * usize::try_from(width)?
                        + usize::try_from(neighbor_x)?;
                    if foreground[neighbor] && !visited[neighbor] {
                        visited[neighbor] = true;
                        stack.push(neighbor);
                    }
                }
            }
        }
        component_sizes.push(size);
    }

    let minimum_significant_pixels = (foreground_count / 100).max(minimum_component_pixels);
    let significant: Vec<usize> = component_sizes
        .into_iter()
        .filter(|size| *size >= minimum_significant_pixels)
        .collect();
    let dominant = significant.iter().copied().max().unwrap_or(0);
    let subject_span_ratio = (f64::from(max_x - min_x + 1) / f64::from(width))
        .max(f64::from(max_y - min_y + 1) / f64::from(height));

    Ok(RawTopology {
        width,
        height,
        significant_components: significant.len(),
        dominant_component_ratio: dominant as f64 / foreground_count as f64,
        subject_span_ratio,
    })
}

fn normalize_icon(
    name: &str,
    source: &RgbaImage,
    canvas: u32,
    target_longest: u32,
    minimum_longest: u32,
) -> Result<RgbaImage, Box<dyn Error>> {
    let bounds = geometry(source)?.bounds;
    let cropped = crop_imm(
        source,
        bounds.min_x,
        bounds.min_y,
        bounds.max_x - bounds.min_x + 1,
        bounds.max_y - bounds.min_y + 1,
    )
    .to_image();
    let mut failures = Vec::new();
    for longest in (minimum_longest..=target_longest).rev() {
        match normalize_cropped_icon(
            name,
            &cropped,
            canvas,
            longest,
            minimum_longest,
            target_longest,
        ) {
            Ok(image) => return Ok(image),
            Err(error) => failures.push(format!("{longest}px: {error}")),
        }
    }
    Err(format!(
        "{name} 在允许的最长边范围内均无法归一化：{}",
        failures.join("；")
    )
    .into())
}

fn normalize_cropped_icon(
    name: &str,
    cropped: &RgbaImage,
    canvas: u32,
    longest_target: u32,
    minimum_longest: u32,
    maximum_longest: u32,
) -> Result<RgbaImage, Box<dyn Error>> {
    let longest = cropped.width().max(cropped.height());
    let resized_width = ((u64::from(cropped.width()) * u64::from(longest_target)
        + u64::from(longest) / 2)
        / u64::from(longest)) as u32;
    let resized_height = ((u64::from(cropped.height()) * u64::from(longest_target)
        + u64::from(longest) / 2)
        / u64::from(longest)) as u32;
    if resized_width.min(resized_height) * 2 < resized_width.max(resized_height) {
        return Err(format!(
            "{name} 等比缩放后的短边仅 {}px，不足最长边 {}px 的一半；必须重新生成，不得拉伸",
            resized_width.min(resized_height),
            resized_width.max(resized_height)
        )
        .into());
    }

    let resized = resize(cropped, resized_width, resized_height, FilterType::Lanczos3);
    let image = center_semantic_on_canvas(name, &resized, canvas)?;
    let bounds = geometry(&image)?.bounds;
    let bbox_width = bounds.max_x - bounds.min_x + 1;
    let bbox_height = bounds.max_y - bounds.min_y + 1;
    let bbox_longest = bbox_width.max(bbox_height);
    let bbox_shortest = bbox_width.min(bbox_height);
    if !(minimum_longest..=maximum_longest).contains(&bbox_longest) {
        return Err(format!(
            "{name} 归一化后 Alpha bbox 为 {bbox_width}x{bbox_height}px，最长边要求 {minimum_longest}-{maximum_longest}px"
        )
        .into());
    }
    if bbox_shortest * 2 < bbox_longest {
        return Err(format!(
            "{name} 归一化后 Alpha bbox 为 {bbox_width}x{bbox_height}px，短边不足最长边的一半；必须重新生成"
        )
        .into());
    }
    validate_occupancy(name, &image)?;
    Ok(image)
}

fn normalize_brand(source: &RgbaImage, canvas: u32) -> Result<RgbaImage, Box<dyn Error>> {
    let bounds = geometry(source)?.bounds;
    let cropped = crop_imm(
        source,
        bounds.min_x,
        bounds.min_y,
        bounds.max_x - bounds.min_x + 1,
        bounds.max_y - bounds.min_y + 1,
    )
    .to_image();
    let longest = cropped.width().max(cropped.height());
    let maximum_longest = canvas - 2;
    let minimum_longest = (canvas * 3).div_ceil(4);
    let mut failures = Vec::new();
    for target_longest in (minimum_longest..=maximum_longest).rev() {
        let width = ((u64::from(cropped.width()) * u64::from(target_longest)
            + u64::from(longest) / 2)
            / u64::from(longest)) as u32;
        let height = ((u64::from(cropped.height()) * u64::from(target_longest)
            + u64::from(longest) / 2)
            / u64::from(longest)) as u32;
        let resized = resize(&cropped, width, height, FilterType::Lanczos3);
        match center_brand_on_canvas("app 品牌图标", &resized, canvas) {
            Ok(image) => return Ok(image),
            Err(error) => failures.push(format!("{target_longest}px: {error}")),
        }
    }
    Err(format!(
        "app 品牌图标在 {canvas}x{canvas} 画布的 75%-最大合法尺寸内均无法满足质心门禁：{}",
        failures.join("；")
    )
    .into())
}

fn center_semantic_on_canvas(
    name: &str,
    source: &RgbaImage,
    canvas: u32,
) -> Result<RgbaImage, Box<dyn Error>> {
    if source.width() + 2 > canvas || source.height() + 2 > canvas {
        return Err(format!("{name} 无法在 {canvas}x{canvas} 画布中保留 1px 透明边距").into());
    }
    let source_geometry = geometry(source)?;
    let center = (f64::from(canvas) - 1.0) / 2.0;
    let base_x = i64::from((canvas - source.width()) / 2);
    let base_y = i64::from((canvas - source.height()) / 2);
    let mut best = None;
    for delta_y in -1_i64..=1 {
        for delta_x in -1_i64..=1 {
            let x = base_x + delta_x;
            let y = base_y + delta_y;
            if x < 1
                || y < 1
                || x + i64::from(source.width()) > i64::from(canvas - 1)
                || y + i64::from(source.height()) > i64::from(canvas - 1)
            {
                continue;
            }
            let deviation_x = (source_geometry.centroid_x + x as f64 - center).abs();
            let deviation_y = (source_geometry.centroid_y + y as f64 - center).abs();
            let score = deviation_x + deviation_y;
            if best
                .as_ref()
                .is_none_or(|(best_score, _, _, _, _)| score < *best_score)
            {
                best = Some((score, deviation_x, deviation_y, x, y));
            }
        }
    }
    let (_, deviation_x, deviation_y, x, y) =
        best.ok_or_else(|| format!("{name} 无法在 {canvas}x{canvas} 画布中保留 1px 透明边距"))?;
    if deviation_x > 0.5 || deviation_y > 0.5 {
        return Err(format!(
            "{name} Alpha 质心偏差为 ({deviation_x:.3},{deviation_y:.3})px，超过 0.5px；必须重新生成"
        )
        .into());
    }
    render_on_canvas(source, canvas, x, y)
}

fn center_brand_on_canvas(
    name: &str,
    source: &RgbaImage,
    canvas: u32,
) -> Result<RgbaImage, Box<dyn Error>> {
    if source.width() + 2 > canvas || source.height() + 2 > canvas {
        return Err(format!("{name} 无法在 {canvas}x{canvas} 画布中保留 1px 透明边距").into());
    }
    let source_geometry = geometry(source)?;
    let center = (f64::from(canvas) - 1.0) / 2.0;
    let maximum_x = canvas - source.width() - 1;
    let maximum_y = canvas - source.height() - 1;
    let mut best = None;
    for y in 1..=maximum_y {
        for x in 1..=maximum_x {
            let deviation_x = (source_geometry.centroid_x + f64::from(x) - center).abs();
            let deviation_y = (source_geometry.centroid_y + f64::from(y) - center).abs();
            let score = deviation_x + deviation_y;
            if best
                .as_ref()
                .is_none_or(|(best_score, _, _, _, _)| score < *best_score)
            {
                best = Some((score, deviation_x, deviation_y, x, y));
            }
        }
    }
    let (_, deviation_x, deviation_y, x, y) =
        best.ok_or_else(|| format!("{name} 无法在 {canvas}x{canvas} 画布中保留 1px 透明边距"))?;
    if deviation_x > 0.5 || deviation_y > 0.5 {
        return Err(format!(
            "{name} Alpha 质心偏差为 ({deviation_x:.3},{deviation_y:.3})px，超过 0.5px；必须重新生成"
        )
        .into());
    }
    render_on_canvas(source, canvas, i64::from(x), i64::from(y))
}

fn render_on_canvas(
    source: &RgbaImage,
    canvas: u32,
    x: i64,
    y: i64,
) -> Result<RgbaImage, Box<dyn Error>> {
    let mut image = RgbaImage::from_pixel(canvas, canvas, Rgba([0, 0, 0, 0]));
    overlay(&mut image, source, x, y);
    for pixel in image.pixels_mut() {
        if pixel.0[3] != 0 {
            pixel.0[0] = 0;
            pixel.0[1] = 0;
            pixel.0[2] = 0;
        }
    }
    Ok(image)
}

fn geometry(image: &RgbaImage) -> Result<Geometry, Box<dyn Error>> {
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
        min_x = min_x.min(x);
        min_y = min_y.min(y);
        max_x = max_x.max(x);
        max_y = max_y.max(y);
        alpha_total += u64::from(alpha);
        weighted_x += f64::from(x) * f64::from(alpha);
        weighted_y += f64::from(y) * f64::from(alpha);
    }
    if alpha_total == 0 {
        return Err("图像没有 Alpha>0 的可见像素".into());
    }
    Ok(Geometry {
        bounds: Bounds {
            min_x,
            min_y,
            max_x,
            max_y,
        },
        centroid_x: weighted_x / alpha_total as f64,
        centroid_y: weighted_y / alpha_total as f64,
        alpha_total,
    })
}

fn validate_occupancy(name: &str, image: &RgbaImage) -> Result<(), Box<dyn Error>> {
    let occupancy = geometry(image)?.alpha_total as f64
        / (f64::from(image.width()) * f64::from(image.height()) * 255.0);
    if !(0.02..=0.60).contains(&occupancy) {
        return Err(format!(
            "{name} Alpha 占用率为 {:.2}%，要求 2%-60%",
            occupancy * 100.0
        )
        .into());
    }
    Ok(())
}

fn encode_ico(png_path: &Path, ico_path: &Path) -> Result<(), Box<dyn Error>> {
    let image = image::open(png_path)?.into_rgba8();
    let file = File::create(ico_path)?;
    IcoEncoder::new(file).write_image(
        image.as_raw(),
        image.width(),
        image.height(),
        ExtendedColorType::Rgba8,
    )?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_significant_multi_component_candidate_sheet() {
        let mut sheet = RgbaImage::from_pixel(128, 128, Rgba([0, 0, 0, 0]));
        for row in 0..4 {
            for column in 0..4 {
                for y in (row * 24 + 4)..(row * 24 + 12) {
                    for x in (column * 24 + 4)..(column * 24 + 12) {
                        sheet.put_pixel(x, y, Rgba([0, 0, 0, 255]));
                    }
                }
            }
        }

        let topology = raw_topology(&sheet).expect("候选表应有可分析的前景");
        assert_eq!(topology.significant_components, 16);
        let error = validate_raw_topology(Path::new("candidate-sheet.png"), topology)
            .expect_err("多枚显著候选必须在归一化前被拒绝");
        assert!(error.to_string().contains("疑似候选表"));
    }
}
