//go:build ignore

package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
)

func main() {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		panic("icon_source_unavailable")
	}
	images := [][]byte{dib(16), dib(32), dib(48)}
	sizes := []int{16, 32, 48}

	var output bytes.Buffer
	write(&output, uint16(0))
	write(&output, uint16(1))
	write(&output, uint16(len(images)))
	offset := uint32(6 + 16*len(images))
	for index, image := range images {
		output.WriteByte(byte(sizes[index]))
		output.WriteByte(byte(sizes[index]))
		output.WriteByte(0)
		output.WriteByte(0)
		write(&output, uint16(1))
		write(&output, uint16(32))
		write(&output, uint32(len(image)))
		write(&output, offset)
		offset += uint32(len(image))
	}
	for _, image := range images {
		output.Write(image)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(source), "icon.ico"), output.Bytes(), 0o644); err != nil {
		panic(err)
	}
}

func dib(size int) []byte {
	maskStride := ((size + 31) / 32) * 4
	pixelBytes := size * size * 4
	maskBytes := maskStride * size
	var image bytes.Buffer
	write(&image, uint32(40))
	write(&image, int32(size))
	write(&image, int32(size*2))
	write(&image, uint16(1))
	write(&image, uint16(32))
	write(&image, uint32(0))
	write(&image, uint32(pixelBytes+maskBytes))
	write(&image, int32(0))
	write(&image, int32(0))
	write(&image, uint32(0))
	write(&image, uint32(0))

	transparent := make([]bool, size*size)
	for y := size - 1; y >= 0; y-- {
		for x := 0; x < size; x++ {
			alpha := byte(255)
			margin := max(1, size/12)
			if x < margin || y < margin || x >= size-margin || y >= size-margin {
				alpha = 0
				transparent[y*size+x] = true
			}
			blue, green, red := byte(205), byte(111), byte(37)
			stroke := max(1, size/8)
			left := size / 4
			right := size - 1 - left
			top := size / 4
			bottom := size - 1 - top
			onM := y >= top && y <= bottom && (abs(x-left) < stroke || abs(x-right) < stroke ||
				abs((x-left)*(bottom-top)-(y-top)*(size/4)) < stroke*(bottom-top) ||
				abs((right-x)*(bottom-top)-(y-top)*(size/4)) < stroke*(bottom-top))
			if onM && alpha != 0 {
				blue, green, red = 245, 245, 245
			}
			image.Write([]byte{blue, green, red, alpha})
		}
	}
	for y := size - 1; y >= 0; y-- {
		row := make([]byte, maskStride)
		for x := 0; x < size; x++ {
			if transparent[y*size+x] {
				row[x/8] |= 0x80 >> uint(x%8)
			}
		}
		image.Write(row)
	}
	return image.Bytes()
}

func write(buffer *bytes.Buffer, value any) {
	if err := binary.Write(buffer, binary.LittleEndian, value); err != nil {
		panic(err)
	}
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
