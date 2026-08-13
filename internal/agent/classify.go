package agent

import (
	"path/filepath"
	"strings"

	"dedup/internal/store"
)

var (
	defaultImageExts = map[string]struct{}{
		".jpg": {}, ".jpeg": {}, ".png": {}, ".webp": {}, ".bmp": {},
		".gif": {}, ".tif": {}, ".tiff": {},
	}
	defaultVideoExts = map[string]struct{}{
		".mp4": {}, ".mkv": {}, ".avi": {}, ".mov": {}, ".wmv": {},
		".flv": {}, ".ts": {}, ".m2ts": {}, ".mpg": {}, ".mpeg": {},
		".webm": {}, ".3gp": {},
	}
)

func MediaKind(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if _, ok := defaultImageExts[ext]; ok {
		return "image"
	}
	if _, ok := defaultVideoExts[ext]; ok {
		return "video"
	}
	return "other"
}

func MediaKindWithExtensions(path string, imageExts, videoExts []string) string {
	if len(imageExts) == 0 && len(videoExts) == 0 {
		return MediaKind(path)
	}
	extension := filepath.Ext(path)
	for _, candidate := range imageExts {
		if strings.EqualFold(extension, candidate) {
			return "image"
		}
	}
	for _, candidate := range videoExts {
		if strings.EqualFold(extension, candidate) {
			return "video"
		}
	}
	return "other"
}

func MissingBase(path string) uint32 {
	return missingBaseForKind(MediaKind(path))
}

func MissingBaseWithExtensions(path string, imageExts, videoExts []string) uint32 {
	return missingBaseForKind(MediaKindWithExtensions(path, imageExts, videoExts))
}

func missingBaseForKind(kind string) uint32 {
	switch kind {
	case "image":
		return store.RequiredStageOneMask(store.MediaImage)
	case "video":
		return store.RequiredStageOneMask(store.MediaVideo)
	default:
		return store.RequiredStageOneMask(store.MediaKind(kind))
	}
}
