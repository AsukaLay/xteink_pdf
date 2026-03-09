package main

import (
	"fmt"
	"image"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/disintegration/imaging"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

type Task struct {
	Path  string
	Index int // 原始顺序索引
}

func main() {
	// 获取当前目录下的所有 .pdf 文件
	pdfs, err := filepath.Glob("*.pdf")
	if err != nil {
		log.Fatal("查找 PDF 文件失败:", err)
	}

	if len(pdfs) == 0 {
		fmt.Println("当前目录下没有找到 PDF 文件。")
		return
	}

	for _, inputPDF := range pdfs {
		// 跳过已经是 _output.pdf 结尾的输出文件，避免重复处理
		if strings.HasSuffix(inputPDF, "_output.pdf") {
			continue
		}

		// 构建输出文件名
		ext := filepath.Ext(inputPDF)
		base := strings.TrimSuffix(inputPDF, ext)
		outputPDF := base + "_output" + ext

		fmt.Printf("\n========== 开始处理: %s ==========\n", inputPDF)
		processSinglePDF(inputPDF, outputPDF)
	}

	// 处理完毕后清理临时目录
	os.RemoveAll("temp_raw")
	os.RemoveAll("temp_cropped")
	fmt.Printf("\n========== 所有 PDF 处理完毕 ==========\n")
}

func processSinglePDF(inputPDF, outputPDF string) {
	tempRaw := "temp_raw"         // 存放提取出的原始图
	tempCropped := "temp_cropped" // 存放切分后的图

	// 清理并创建目录
	os.RemoveAll(tempRaw)
	os.RemoveAll(tempCropped)
	os.MkdirAll(tempRaw, 0755)
	os.MkdirAll(tempCropped, 0755)

	// 1. 提取图片
	fmt.Println("Step 1: 正在从 PDF 提取图片...")
	if err := api.ExtractImagesFile(inputPDF, tempRaw, nil, nil); err != nil {
		log.Printf("提取失败跳过此文件: %v\n", err)
		return
	}

	// 2. 获取并排序提取的文件
	files, _ := filepath.Glob(filepath.Join(tempRaw, "*.*"))
	sort.Strings(files)

	// 3. 并发处理切割
	tasks := make(chan Task, len(files))
	var wg sync.WaitGroup
	numWorkers := runtime.NumCPU()

	fmt.Printf("Step 2: 启动 %d 个 Worker 进行白边检测切分...\n", numWorkers)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range tasks {
				processMangaPage(t, tempCropped)
			}
		}()
	}

	for i, f := range files {
		// 只处理图片
		ext := strings.ToLower(filepath.Ext(f))
		if ext == ".jpg" || ext == ".png" || ext == ".jpeg" {
			tasks <- Task{Path: f, Index: i}
		}
	}
	close(tasks)
	wg.Wait()

	// 4. 合成最终 PDF
	fmt.Println("Step 3: 正在重新合成 PDF...")
	finalImgs, _ := filepath.Glob(filepath.Join(tempCropped, "*.jpg"))
	if len(finalImgs) == 0 {
		fmt.Println("未找到任何切分后的图片，跳过合成。")
		return
	}
	sort.Strings(finalImgs) // 确保 0001_01 在 0001_02 前面

	impConf := pdfcpu.DefaultImportConfig()
	impConf.Pos = types.Full // 图片铺满页面
	if err := api.ImportImagesFile(finalImgs, outputPDF, impConf, nil); err != nil {
		log.Printf("合成失败: %v\n", err)
		return
	}

	fmt.Printf("成功！当前 PDF 已保存为: %s\n", outputPDF)
}

func processMangaPage(t Task, outDir string) {
	src, err := imaging.Open(t.Path)
	if err != nil {
		return
	}

	bounds := src.Bounds()
	// 找到所有水平白边位置
	gaps := findHorizontalGaps(src)

	lastY := bounds.Min.Y
	// 加上终点坐标，方便循环切割
	gaps = append(gaps, bounds.Max.Y)

	partIdx := 0
	for _, y := range gaps {
		height := y - lastY
		// 过滤太小的碎片（比如只有 20 像素高的白边）
		if height < 150 {
			continue
		}

		rect := image.Rect(bounds.Min.X, lastY, bounds.Max.X, y)
		cropImg := imaging.Crop(src, rect)

		// 命名格式：原页码索引(4位)_切块索引(2位).jpg
		outPath := filepath.Join(outDir, fmt.Sprintf("%04d_%02d.jpg", t.Index, partIdx))
		imaging.Save(cropImg, outPath)

		lastY = y
		partIdx++
	}
}

func findHorizontalGaps(img image.Image) []int {
	bounds := img.Bounds()
	var gaps []int
	whiteThreshold := uint32(63000) // 接近 65535 (16bit RGBA 的纯白)

	inGap := false
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		isWhiteLine := true
		// 采样检测：每隔 10 个像素检测一个点
		for x := bounds.Min.X; x < bounds.Max.X; x += 10 {
			r, g, b, _ := img.At(x, y).RGBA()
			if r < whiteThreshold || g < whiteThreshold || b < whiteThreshold {
				isWhiteLine = false
				break
			}
		}

		if isWhiteLine && !inGap {
			gaps = append(gaps, y)
			inGap = true
		} else if !isWhiteLine {
			inGap = false
		}
	}
	return gaps
}
