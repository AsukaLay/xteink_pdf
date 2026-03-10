package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// extractMobiImages extracts all JPEG/PNG/GIF images from a MOBI/AZW3 file (PalmDB format)
// and saves them to the outDir.
func extractMobiImages(mobiPath, outDir string) error {
	f, err := os.Open(mobiPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// 1. 读取 PalmDB Header (78 bytes)
	header := make([]byte, 78)
	if n, err := io.ReadFull(f, header); err != nil || n < 78 {
		return fmt.Errorf("读取 PDB Header 失败: %w", err)
	}

	// 记录数量在第 76 字节 (2 字节, 大端)
	numRecords := int(binary.BigEndian.Uint16(header[76:78]))
	if numRecords == 0 {
		return fmt.Errorf("找不到任何记录")
	}

	// 2. 读取记录头表，每个记录 8 字节
	recordInfoList := make([]byte, numRecords*8)
	if _, err := io.ReadFull(f, recordInfoList); err != nil {
		return fmt.Errorf("读取记录表失败: %w", err)
	}

	// 获取所有记录的偏移量
	offsets := make([]uint32, numRecords)
	for i := 0; i < numRecords; i++ {
		// 取前 4 字节的偏移量
		offsets[i] = binary.BigEndian.Uint32(recordInfoList[i*8 : i*8+4])
	}

	// 获取文件总大小，作为最后一个记录的终止位置
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	fileSize := uint32(fi.Size())

	imgIndex := 1
	for i := 0; i < numRecords; i++ {
		start := offsets[i]
		var end uint32
		if i < numRecords-1 {
			end = offsets[i+1]
		} else {
			end = fileSize
		}

		// 处理乱序或者不规则偏移量带来的负值
		if start >= end || end > fileSize {
			continue
		}

		size := end - start
		if size < 32 {
			continue // 太小，不可能是有效图片
		}

		// 跳转到记录开始位置
		if _, err := f.Seek(int64(start), io.SeekStart); err != nil {
			continue
		}

		// 读取记录数据
		imgData := make([]byte, size)
		if _, err := io.ReadFull(f, imgData); err != nil {
			continue
		}

		// 判断 Magic Bytes 寻找图片
		ext := ""
		if len(imgData) > 12 {
			if imgData[0] == 0xFF && imgData[1] == 0xD8 && imgData[2] == 0xFF {
				ext = ".jpg"
			} else if bytes.HasPrefix(imgData, []byte("\x89PNG\r\n\x1a\n")) {
				ext = ".png"
			} else if bytes.HasPrefix(imgData, []byte("GIF87a")) || bytes.HasPrefix(imgData, []byte("GIF89a")) {
				ext = ".gif"
			}
		}

		if ext != "" {
			outPath := filepath.Join(outDir, fmt.Sprintf("img_%04d%s", imgIndex, ext))
			os.WriteFile(outPath, imgData, 0644)
			imgIndex++
		}
	}

	if imgIndex == 1 {
		return fmt.Errorf("没有在此文件中找到任何图片")
	}

	fmt.Printf("成功从 MOBI/AZW3 提取了 %d 张图片\n", imgIndex-1)
	return nil
}
