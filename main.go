package main

import (
	"archive/zip"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

type Task struct {
	Path  string
	Index int // 原始顺序索引
}

type WebTask struct {
	ID        string
	Status    string // "processing", "done", "error"
	ErrorMsg  string
	CreatedAt time.Time
	FileName  string // 原始文件名
	OutFile   string // 输出PDF绝对路径
}

var (
	webTasks      = make(map[string]*WebTask)
	webTasksMutex sync.Mutex
	webTasksDir   = "web_tasks_data"
)

func main() {
	inDir := flag.String("in", "", "输入目录 (留空则启动 Web 界面)")
	outDir := flag.String("out", ".", "输出目录 (命令行模式用)")
	port := flag.String("port", "80", "Web 服务端口")
	flag.Parse()

	if *inDir != "" {
		runCLI(*inDir, *outDir)
	} else {
		runWebServer(*port)
	}
}

func runCLI(inDir, outDir string) {
	// 确保输出目录存在
	if err := os.MkdirAll(outDir, 0755); err != nil {
		log.Fatal("无法创建输出目录:", err)
	}

	var inputFiles []string
	entries, err := os.ReadDir(inDir)
	if err != nil {
		log.Fatalf("读取输入目录 %s 失败: %v", inDir, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".pdf" || ext == ".mobi" || ext == ".azw3" {
			// 跳过已经是 _output 结尾的输出文件，避免重复处理
			if strings.HasSuffix(strings.ToLower(e.Name()), "_output.pdf") {
				continue
			}
			inputFiles = append(inputFiles, e.Name())
		}
	}

	if len(inputFiles) == 0 {
		fmt.Printf("目录 %s 下没有找到可处理的文件 (PDF/MOBI/AZW3)。\n", inDir)
		return
	}

	for _, inputFile := range inputFiles {
		inputPath := filepath.Join(inDir, inputFile)
		// 构建输出文件名，统一输出 PDF
		ext := filepath.Ext(inputFile)
		base := strings.TrimSuffix(inputFile, ext)
		outputPDF := filepath.Join(outDir, base+"_output.pdf")

		fmt.Printf("\n========== 开始处理: %s ==========\n", inputFile)
		if err := processSingleFile(inputPath, outputPDF); err != nil {
			fmt.Printf("处理失败: %v\n", err)
		}
	}

	fmt.Printf("\n========== 所有文件处理完毕 ==========\n")
}

func processSingleFile(inputFile, outputPDF string) error {
	tempRaw, err := os.MkdirTemp("", "temp_raw_*")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempRaw)

	tempCropped, err := os.MkdirTemp("", "temp_cropped_*")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempCropped)

	// 1. 提取图片
	ext := strings.ToLower(filepath.Ext(inputFile))
	fmt.Printf("Step 1: 正在从 %s 提取图片...\n", strings.ToUpper(ext[1:]))

	if ext == ".pdf" {
		if err := api.ExtractImagesFile(inputFile, tempRaw, nil, nil); err != nil {
			return fmt.Errorf("提取 PDF 图片失败: %v", err)
		}
	} else if ext == ".mobi" || ext == ".azw3" {
		if err := extractMobiImages(inputFile, tempRaw); err != nil {
			return fmt.Errorf("提取 MOBI/AZW3 图片失败: %v", err)
		}
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
		return fmt.Errorf("未找到任何切分后的图片")
	}
	sort.Strings(finalImgs) // 确保 0001_01 在 0001_02 前面

	impConf := pdfcpu.DefaultImportConfig()
	impConf.Pos = types.Full // 图片铺满页面
	if err := api.ImportImagesFile(finalImgs, outputPDF, impConf, nil); err != nil {
		return fmt.Errorf("合成 PDF 失败: %v", err)
	}

	fmt.Printf("成功！当前 PDF 已保存为: %s\n", outputPDF)
	return nil
}

// processSmartSplitFile extracts images, then for each image estimates rendered height
// on a 600px-wide reader. If rendered height > 1600px, saves the original image first,
// then appends the split parts immediately after. Finally compiles all into a PDF.
func processSmartSplitFile(inputFile, outputPDF string, keepOriginal bool) error {
	tempRaw, err := os.MkdirTemp("", "temp_raw_*")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempRaw)

	tempOut, err := os.MkdirTemp("", "temp_smart_*")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempOut)

	ext := strings.ToLower(filepath.Ext(inputFile))
	fmt.Printf("Step 1: 正在从 %s 提取图片...\n", strings.ToUpper(ext[1:]))

	if ext == ".pdf" {
		if err := api.ExtractImagesFile(inputFile, tempRaw, nil, nil); err != nil {
			return fmt.Errorf("提取 PDF 图片失败: %v", err)
		}
	} else if ext == ".mobi" || ext == ".azw3" {
		if err := extractMobiImages(inputFile, tempRaw); err != nil {
			return fmt.Errorf("提取 MOBI/AZW3 图片失败: %v", err)
		}
	}

	files, _ := filepath.Glob(filepath.Join(tempRaw, "*.*"))
	sort.Strings(files)
	totalFiles := len(files)
	fmt.Printf("Step 2: 智能字体检测切分 (%d 张图片)...\n", totalFiles)

	outSeq := 0
	for idx, f := range files {
		if (idx+1)%20 == 0 || idx == totalFiles-1 {
			fmt.Printf("Step 2 进度: [%d/%d] (%.1f%%)...\n", idx+1, totalFiles, float64(idx+1)/float64(totalFiles)*100)
		}
		fext := strings.ToLower(filepath.Ext(f))
		if fext != ".jpg" && fext != ".png" && fext != ".jpeg" {
			os.Remove(f)
			continue
		}
		src, err := imaging.Open(f)
		if err != nil {
			os.Remove(f)
			continue
		}
		bounds := src.Bounds()
		w := bounds.Dx()
		h := bounds.Dy()
		if w == 0 {
			src = nil
			os.Remove(f)
			continue
		}

		// Estimate rendered height on a 600px-wide reader
		renderedHeight := (600.0 / float64(w)) * float64(h)
		_ = renderedHeight // used per-segment below

		// Save original image if keepOriginal is set
		if keepOriginal {
			origPath := filepath.Join(tempOut, fmt.Sprintf("%06d_orig.jpg", outSeq))
			imaging.Save(src, origPath)
			outSeq++
		}

		// Split on every white gap found; if a resulting segment is still too tall, split it further
		gaps := findHorizontalGaps(src)
		gaps = append(gaps, bounds.Max.Y)
		lastY := bounds.Min.Y
		for _, gapY := range gaps {
			segH := gapY - lastY
			if segH < 50 {
				// skip tiny slivers but advance lastY only if we'd end up at a real gap
				continue
			}
			// If segment is still taller than one screen on a 600px reader, split it further
			segRendered := (600.0 / float64(w)) * float64(segH)
			if segRendered > 800 {
				nParts := int(segRendered/800) + 1
				cuts := findBestCutLines(imaging.Crop(src, image.Rect(bounds.Min.X, lastY, bounds.Max.X, gapY)), nParts)
				prevY := lastY
				for _, c := range cuts {
					absC := lastY + c
					if absC-prevY < 50 {
						continue
					}
					p := filepath.Join(tempOut, fmt.Sprintf("%06d_split.jpg", outSeq))
					imaging.Save(imaging.Crop(src, image.Rect(bounds.Min.X, prevY, bounds.Max.X, absC)), p)
					outSeq++
					prevY = absC
				}
				if gapY-prevY >= 50 {
					p := filepath.Join(tempOut, fmt.Sprintf("%06d_split.jpg", outSeq))
					imaging.Save(imaging.Crop(src, image.Rect(bounds.Min.X, prevY, bounds.Max.X, gapY)), p)
					outSeq++
				}
			} else {
				p := filepath.Join(tempOut, fmt.Sprintf("%06d_split.jpg", outSeq))
				imaging.Save(imaging.Crop(src, image.Rect(bounds.Min.X, lastY, bounds.Max.X, gapY)), p)
				outSeq++
			}
			lastY = gapY
		}

		src = nil
		os.Remove(f)
		if (idx+1)%15 == 0 {
			runtime.GC()
			debug.FreeOSMemory()
		}
	}

	fmt.Println("Step 3: 正在合成 PDF...")
	finalImgs, _ := filepath.Glob(filepath.Join(tempOut, "*.jpg"))
	if len(finalImgs) == 0 {
		return fmt.Errorf("未找到任何图片")
	}
	sort.Strings(finalImgs)

	impConf := pdfcpu.DefaultImportConfig()
	impConf.Pos = types.Full
	if err := api.ImportImagesFile(finalImgs, outputPDF, impConf, nil); err != nil {
		return fmt.Errorf("合成 PDF 失败: %v", err)
	}
	fmt.Printf("智能切分完成，PDF 已保存: %s\n", outputPDF)
	return nil
}

// drawStatusBarText draws a progress/page status bar at the bottom 20px of the image
func drawStatusBarText(img *image.RGBA, text string, width, height int) {
	barY := height - 20
	for x := 0; x < width; x++ {
		img.Set(x, barY, color.Black)
	}

	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.Black),
		Face: basicfont.Face7x13,
	}
	textWidth := d.MeasureString(text).Ceil()
	textX := (width - textWidth) / 2
	if textX < 0 {
		textX = 0
	}
	textY := barY + 14

	d.Dot = fixed.P(textX, textY)
	d.DrawString(text)
}

// encodeXTG encodes an image.Image into XTG format (22-byte header + 1bpp bitmap)
func encodeXTG(img image.Image, width, height int, dither bool) []byte {
	rowBytes := (width + 7) / 8
	bitmapSize := rowBytes * height

	header := make([]byte, 22)
	header[0] = 'X'
	header[1] = 'T'
	header[2] = 'G'
	header[3] = 0x00

	binary.LittleEndian.PutUint16(header[4:6], uint16(width))
	binary.LittleEndian.PutUint16(header[6:8], uint16(height))
	header[8] = 0 // colorMode = 0
	header[9] = 0 // compression = 0

	binary.LittleEndian.PutUint32(header[10:14], uint32(bitmapSize))

	bitmap := make([]byte, bitmapSize)
	grayBuf := make([]float32, width*height)
	bounds := img.Bounds()

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var r, g, b uint32
			if x >= bounds.Min.X && x < bounds.Max.X && y >= bounds.Min.Y && y < bounds.Max.Y {
				r, g, b, _ = img.At(x, y).RGBA()
			} else {
				r, g, b = 65535, 65535, 65535
			}
			lum := 0.299*float32(r>>8) + 0.587*float32(g>>8) + 0.114*float32(b>>8)
			grayBuf[y*width+x] = lum
		}
	}

	if dither {
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				idx := y*width + x
				oldVal := grayBuf[idx]
				newVal := float32(0)
				if oldVal >= 128 {
					newVal = 255
				}
				grayBuf[idx] = newVal
				err := (oldVal - newVal)

				if x+1 < width {
					grayBuf[idx+1] += err * (7.0 / 16.0)
				}
				if y+1 < height {
					if x > 0 {
						grayBuf[idx+width-1] += err * (3.0 / 16.0)
					}
					grayBuf[idx+width] += err * (5.0 / 16.0)
					if x+1 < width {
						grayBuf[idx+width+1] += err * (1.0 / 16.0)
					}
				}
			}
		}
	}

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			idx := y*width + x
			if grayBuf[idx] >= 128 {
				byteIdx := y*rowBytes + (x / 8)
				bitIdx := 7 - (x % 8)
				bitmap[byteIdx] |= (1 << bitIdx)
			}
		}
	}

	result := make([]byte, len(header)+len(bitmap))
	copy(result[0:], header)
	copy(result[len(header):], bitmap)
	return result
}

// buildXTCContainer creates official 56-byte container header + 256-byte metadata + index table + page data
// orient: 0=竖屏, 90=横屏（X4 设备通过 buf[8] 字节识别横还是竖屏）
func buildXTCContainer(xtgPages [][]byte, title string, width, height int, orient int) []byte {
	pageCount := len(xtgPages)
	headerSize := 56
	metadataSize := 256
	indexEntrySize := 16
	indexSize := pageCount * indexEntrySize

	metadataOffset := headerSize
	chapterOffset := metadataOffset + metadataSize
	indexOffset := chapterOffset
	pageDataOffset := indexOffset + indexSize

	pageOffsets := make([]uint64, pageCount)
	pageSizes := make([]uint32, pageCount)
	currOff := uint64(pageDataOffset)
	for i, page := range xtgPages {
		pageOffsets[i] = currOff
		pageSizes[i] = uint32(len(page))
		currOff += uint64(len(page))
	}

	totalSize := currOff
	buf := make([]byte, totalSize)

	copy(buf[0:4], []byte("XTC\x00"))
	binary.LittleEndian.PutUint16(buf[4:6], 1)
	binary.LittleEndian.PutUint16(buf[6:8], uint16(pageCount))
	// buf[8] readDirection: 与 epub-to-xtc-converter 保持一致写 0。
	// X4 设备依据页面索引中的宽高(800>480)自动判断横屏显示。
	buf[8] = 0
	buf[9] = 1
	buf[10] = 0
	buf[11] = 0
	binary.LittleEndian.PutUint32(buf[12:16], 1)

	binary.LittleEndian.PutUint64(buf[16:24], uint64(metadataOffset))
	binary.LittleEndian.PutUint64(buf[24:32], uint64(indexOffset))
	binary.LittleEndian.PutUint64(buf[32:40], uint64(pageDataOffset))
	binary.LittleEndian.PutUint64(buf[40:48], 0)
	binary.LittleEndian.PutUint64(buf[48:56], uint64(chapterOffset))

	if len(title) > 126 {
		title = title[:126]
	}
	copy(buf[metadataOffset:], []byte(title))
	binary.LittleEndian.PutUint32(buf[metadataOffset+192:metadataOffset+196], uint32(time.Now().Unix()))

	idxPos := indexOffset
	for i := 0; i < pageCount; i++ {
		binary.LittleEndian.PutUint64(buf[idxPos:idxPos+8], pageOffsets[i])
		binary.LittleEndian.PutUint32(buf[idxPos+8:idxPos+12], pageSizes[i])
		binary.LittleEndian.PutUint16(buf[idxPos+12:idxPos+14], uint16(width))
		binary.LittleEndian.PutUint16(buf[idxPos+14:idxPos+16], uint16(height))
		idxPos += indexEntrySize
	}

	dataPos := pageDataOffset
	for _, page := range xtgPages {
		copy(buf[dataPos:], page)
		dataPos += len(page)
	}

	return buf
}

// devicePreset 定义目标设备的分辨率预设。底部 20px 用作页码状态栏。
// X4 = 480x800，X3 = 528x792，eego = 552x768。
type devicePreset struct {
	Name          string
	Width         int
	Height        int
	ContentHeight int
}

var devicePresets = map[string]devicePreset{
	"x4":      {Name: "阅星曈X4", Width: 480, Height: 800, ContentHeight: 780},
	"x3":      {Name: "阅星曈X3", Width: 528, Height: 792, ContentHeight: 772},
	"eego":    {Name: "eego", Width: 552, Height: 768, ContentHeight: 748},
	"eego_a4": {Name: "eego", Width: 552, Height: 768, ContentHeight: 748},
	"a4":      {Name: "eego", Width: 552, Height: 768, ContentHeight: 748},
}

func resolveDevice(key string) devicePreset {
	key = strings.ToLower(strings.TrimSpace(key))
	if p, ok := devicePresets[key]; ok {
		return p
	}
	return devicePresets["x4"]
}

// processSmartSplitToXTC extracts images, splits them intelligently,
// and encodes into Xteink XTC format (1-bit Dither, Status Bar).
// device: "x4" (480x800)、"x3" (528x792) 或 "eego_a4" (552x768)。
func processSmartSplitToXTC(inputFile, outputXTC string, keepOriginal bool, orient int, device string) error {
	tempRaw, err := os.MkdirTemp("", "temp_raw_*")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempRaw)

	tempOut, err := os.MkdirTemp("", "temp_smart_*")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempOut)

	ext := strings.ToLower(filepath.Ext(inputFile))
	fmt.Printf("[XTC] Step 1: 正在从 %s 提取图片...\n", strings.ToUpper(ext[1:]))

	if ext == ".pdf" {
		if err := api.ExtractImagesFile(inputFile, tempRaw, nil, nil); err != nil {
			return fmt.Errorf("提取 PDF 图片失败: %v", err)
		}
	} else if ext == ".mobi" || ext == ".azw3" {
		if err := extractMobiImages(inputFile, tempRaw); err != nil {
			return fmt.Errorf("提取 MOBI/AZW3 图片失败: %v", err)
		}
	}

	files, _ := filepath.Glob(filepath.Join(tempRaw, "*.*"))
	sort.Strings(files)
	totalFiles := len(files)
	fmt.Printf("[XTC] Step 2: 智能字体检测切分 (%d 张图片)...\n", totalFiles)

	outSeq := 0
	for idx, f := range files {
		if (idx+1)%20 == 0 || idx == totalFiles-1 {
			fmt.Printf("[XTC] Step 2 进度: [%d/%d] (%.1f%%)...\n", idx+1, totalFiles, float64(idx+1)/float64(totalFiles)*100)
		}
		fext := strings.ToLower(filepath.Ext(f))
		if fext != ".jpg" && fext != ".png" && fext != ".jpeg" {
			os.Remove(f)
			continue
		}
		src, err := imaging.Open(f)
		if err != nil {
			os.Remove(f)
			continue
		}
		bounds := src.Bounds()
		w := bounds.Dx()
		h := bounds.Dy()
		_ = h
		if w == 0 {
			src = nil
			os.Remove(f)
			continue
		}

		if keepOriginal {
			origPath := filepath.Join(tempOut, fmt.Sprintf("%06d_orig.jpg", outSeq))
			imaging.Save(src, origPath)
			outSeq++
		}

		gaps := findHorizontalGaps(src)
		gaps = append(gaps, bounds.Max.Y)
		lastY := bounds.Min.Y
		for _, gapY := range gaps {
			segH := gapY - lastY
			if segH < 50 {
				continue
			}
			segRendered := (600.0 / float64(w)) * float64(segH)
			if segRendered > 800 {
				nParts := int(segRendered/800) + 1
				cuts := findBestCutLines(imaging.Crop(src, image.Rect(bounds.Min.X, lastY, bounds.Max.X, gapY)), nParts)
				prevY := lastY
				for _, c := range cuts {
					absC := lastY + c
					if absC-prevY < 50 {
						continue
					}
					p := filepath.Join(tempOut, fmt.Sprintf("%06d_split.jpg", outSeq))
					imaging.Save(imaging.Crop(src, image.Rect(bounds.Min.X, prevY, bounds.Max.X, absC)), p)
					outSeq++
					prevY = absC
				}
				if gapY-prevY >= 50 {
					p := filepath.Join(tempOut, fmt.Sprintf("%06d_split.jpg", outSeq))
					imaging.Save(imaging.Crop(src, image.Rect(bounds.Min.X, prevY, bounds.Max.X, gapY)), p)
					outSeq++
				}
			} else {
				p := filepath.Join(tempOut, fmt.Sprintf("%06d_split.jpg", outSeq))
				imaging.Save(imaging.Crop(src, image.Rect(bounds.Min.X, lastY, bounds.Max.X, gapY)), p)
				outSeq++
			}
			lastY = gapY
		}

		src = nil
		os.Remove(f)
		if (idx+1)%15 == 0 {
			runtime.GC()
			debug.FreeOSMemory()
		}
	}

	finalImgs, _ := filepath.Glob(filepath.Join(tempOut, "*.jpg"))
	if len(finalImgs) == 0 {
		return fmt.Errorf("未找到任何图片")
	}
	sort.Strings(finalImgs)

	preset := resolveDevice(device)
	targetWidth := preset.Width
	targetHeight := preset.Height
	contentHeight := preset.ContentHeight

	fmt.Printf("[XTC] Step 3: 正在转为 %s XTC 格式 (%dx%d, Orient=%d, 1-bit Dither, 状态栏)...\n", preset.Name, targetWidth, targetHeight, orient)

	var xtgPages [][]byte
	total := len(finalImgs)

	for idx, imgPath := range finalImgs {
		if (idx+1)%50 == 0 || idx == total-1 {
			fmt.Printf("[XTC] Step 3 进度: [%d/%d] (%.1f%%)...\n", idx+1, total, float64(idx+1)/float64(total)*100)
		}
		src, err := imaging.Open(imgPath)
		if err != nil {
			continue
		}

		// 不对图片做额外旋转：直接将漫画内容缩放适配到目标尺寸 (%dx%d)。
		// 威新X4通过判断页面宽>高自动识别横屏显示。
		canvas := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
		for i := 0; i < len(canvas.Pix); i += 4 {
			canvas.Pix[i] = 255
			canvas.Pix[i+1] = 255
			canvas.Pix[i+2] = 255
			canvas.Pix[i+3] = 255
		}

		bounds := src.Bounds()
		sw := bounds.Dx()
		sh := bounds.Dy()
		if sw > 0 && sh > 0 {
			// 方案A：如果用户选择了横屏模式（90或270），且当前页面宽 > 高（横图），
			// 则根据旋转方向对图片进行物理旋转，使其变为竖屏比例（480x800），防止硬件花屏。
			if sw > sh && (orient == 90 || orient == 270) {
				if orient == 90 {
					src = imaging.Rotate90(src)
				} else {
					src = imaging.Rotate270(src)
				}
				bounds = src.Bounds()
				sw = bounds.Dx()
				sh = bounds.Dy()
			}

			scaleX := float64(targetWidth) / float64(sw)
			scaleY := float64(contentHeight) / float64(sh)
			scale := scaleX
			if scaleY < scale {
				scale = scaleY
			}
			newW := int(float64(sw) * scale)
			newH := int(float64(sh) * scale)
			if newW < 1 {
				newW = 1
			}
			if newH < 1 {
				newH = 1
			}

			resized := imaging.Resize(src, newW, newH, imaging.Lanczos)
			offsetX := (targetWidth - newW) / 2
			offsetY := (contentHeight - newH) / 2

			for y := 0; y < newH; y++ {
				for x := 0; x < newW; x++ {
					destX := offsetX + x
					destY := offsetY + y
					if destX >= 0 && destX < targetWidth && destY >= 0 && destY < contentHeight {
						canvas.Set(destX, destY, resized.At(x, y))
					}
				}
			}
			resized = nil
		}
		src = nil

		statusText := fmt.Sprintf("%d / %d", idx+1, total)
		drawStatusBarText(canvas, statusText, targetWidth, targetHeight)

		xtg := encodeXTG(canvas, targetWidth, targetHeight, true)
		xtgPages = append(xtgPages, xtg)
		canvas = nil

		if (idx+1)%25 == 0 {
			runtime.GC()
			debug.FreeOSMemory()
		}
	}

	baseName := strings.TrimSuffix(filepath.Base(inputFile), filepath.Ext(inputFile))
	xtcBytes := buildXTCContainer(xtgPages, baseName, targetWidth, targetHeight, orient)

	if err := os.WriteFile(outputXTC, xtcBytes, 0644); err != nil {
		return fmt.Errorf("写入 XTC 文件失败: %v", err)
	}

	orientation := fmt.Sprintf("标准竖屏 %dx%d", targetWidth, targetHeight)
	if orient == 90 || orient == 270 {
		orientation = fmt.Sprintf("自适应旋转横屏 %dx%d", targetWidth, targetHeight)
	}
	fmt.Printf("[XTC] 转换完成！已导出 %s XTC 文件: %s (共 %d 页)\n", orientation, outputXTC, len(xtgPages))
	return nil
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

		// 如果切出来的图片非常长（高度大于宽度的2倍或指定阈值），
		// 会导致在阅读器上被缩小，从而字体太小，因此强行切成两半
		width := bounds.Dx()
		if height > width*2 {
			halfY := lastY + height/2

			// 保存上半部分
			rect1 := image.Rect(bounds.Min.X, lastY, bounds.Max.X, halfY)
			cropImg1 := imaging.Crop(src, rect1)
			outPath1 := filepath.Join(outDir, fmt.Sprintf("%04d_%02d.jpg", t.Index, partIdx))
			imaging.Save(cropImg1, outPath1)
			partIdx++

			// 保存下半部分
			rect2 := image.Rect(bounds.Min.X, halfY, bounds.Max.X, y)
			cropImg2 := imaging.Crop(src, rect2)
			outPath2 := filepath.Join(outDir, fmt.Sprintf("%04d_%02d.jpg", t.Index, partIdx))
			imaging.Save(cropImg2, outPath2)
			partIdx++
		} else {
			rect := image.Rect(bounds.Min.X, lastY, bounds.Max.X, y)
			cropImg := imaging.Crop(src, rect)

			// 命名格式：原页码索引(4位)_切块索引(2位).jpg
			outPath := filepath.Join(outDir, fmt.Sprintf("%04d_%02d.jpg", t.Index, partIdx))
			imaging.Save(cropImg, outPath)
			partIdx++
		}

		lastY = y
	}
}

// isTextDense returns true if the image has high text density.
// It estimates text by counting horizontal color-transition pairs per row
// (text edges produce many short dark↔light transitions).
// Returns the ratio of text-dense rows to total rows.
func isTextDense(img image.Image) float64 {
	bounds := img.Bounds()
	totalRows := bounds.Max.Y - bounds.Min.Y
	if totalRows == 0 {
		return 0
	}
	denseRows := 0
	darkThreshold := uint32(30000) // dark pixel: any channel < ~46% of max
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		transitions := 0
		prevDark := false
		for x := bounds.Min.X; x < bounds.Max.X; x += 3 {
			r, g, b, _ := img.At(x, y).RGBA()
			isDark := r < darkThreshold || g < darkThreshold || b < darkThreshold
			if isDark != prevDark {
				transitions++
			}
			prevDark = isDark
		}
		// A text row typically has many short transitions; threshold ~20 per row
		if transitions > 20 {
			denseRows++
		}
	}
	return float64(denseRows) / float64(totalRows)
}

// findBestCutLines finds up to maxCuts horizontal cut points by scanning for
// rows with the fewest non-white pixels (minimum content rows), searching in
// bands between minY and maxY divided into nParts equal segments.
func findBestCutLines(img image.Image, nParts int) []int {
	bounds := img.Bounds()
	h := bounds.Max.Y - bounds.Min.Y
	if nParts < 2 || h < 300 {
		return nil
	}
	// Count non-white pixels per row (sampled every 5px)
	whiteThreshold := uint32(55000)
	rowDark := make([]int, h)
	for y := 0; y < h; y++ {
		count := 0
		for x := bounds.Min.X; x < bounds.Max.X; x += 5 {
			r, g, b, _ := img.At(x, bounds.Min.Y+y).RGBA()
			if r < whiteThreshold || g < whiteThreshold || b < whiteThreshold {
				count++
			}
		}
		rowDark[y] = count
	}

	var cuts []int
	segH := h / nParts
	// For each internal segment boundary, find the minimum-dark row within ±20% of segH
	for i := 1; i < nParts; i++ {
		center := i * segH
		searchStart := center - segH/5
		searchEnd := center + segH/5
		if searchStart < 0 {
			searchStart = 0
		}
		if searchEnd >= h {
			searchEnd = h - 1
		}
		bestY := center
		bestVal := rowDark[center]
		for y := searchStart; y <= searchEnd; y++ {
			if rowDark[y] < bestVal {
				bestVal = rowDark[y]
				bestY = y
			}
		}
		cuts = append(cuts, bounds.Min.Y+bestY)
	}
	return cuts
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

// ============== Web 服务相关代码 ==============

func initWebTasksDir() {
	// 每次启动时清空之前的任务文件夹
	os.RemoveAll(webTasksDir)
	err := os.MkdirAll(webTasksDir, 0755)
	if err != nil {
		log.Fatalf("无法创建 Web 任务目录: %v", err)
	}

	// 启动定期清理协程 (每 5 分钟检查一次，清理超过 20 分钟的任务)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		for range ticker.C {
			cleanExpiredTasks()
		}
	}()
}

func cleanExpiredTasks() {
	webTasksMutex.Lock()
	defer webTasksMutex.Unlock()

	now := time.Now()
	for id, task := range webTasks {
		// 如果任务创建超过 20 分钟
		if now.Sub(task.CreatedAt) > 20*time.Minute {
			fmt.Printf("[清理] 任务 %s 已过期，正在删除相关文件...\n", id)
			// 删除任务对应的独立文件夹
			os.RemoveAll(filepath.Join(webTasksDir, id))
			delete(webTasks, id)
		}
	}
}

const indexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>漫画自动切去白边与转换</title>
    <style>
        body { 
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif; 
            background: linear-gradient(135deg, #f5f7fa 0%, #c3cfe2 100%);
            display: flex; justify-content: center; align-items: flex-start; 
            min-height: 100vh; margin: 0; color: #333; padding-top: 5vh; padding-bottom: 5vh;
        }
        .card { 
            background: rgba(255, 255, 255, 0.95); 
            padding: 2.5rem 1.8rem; 
            border-radius: 16px; 
            box-shadow: 0 10px 30px rgba(0,0,0,0.1); 
            text-align: center; width: 90%; max-width: 540px; 
            backdrop-filter: blur(10px);
        }
        h2 { margin-top: 0; color: #2c3e50; font-size: 1.6rem; margin-bottom: 1.5rem; }
        .file-input-wrapper { margin: 1.5rem 0; }
        input[type="file"] { display: none; }
        .upload-btn { 
            background: #eef2f5; color: #2c3e50; 
            padding: 14px 24px; border-radius: 8px; 
            cursor: pointer; display: inline-block; 
            font-weight: 600; font-size: 1rem;
            border: 2px dashed #bdc3c7;
            transition: all 0.3s ease;
            width: 90%; box-sizing: border-box;
            word-break: break-all;
        }
        .upload-btn:hover { border-color: #3498db; color: #3498db; background: #f8faff; }
        #submitBtn, #submitSmartBtn, #submitSmartXTCBtn, #downloadAllBtn {
            background: #3498db; color: white; border: none; 
            padding: 14px 24px; border-radius: 8px; 
            cursor: pointer; font-size: 1.05rem; font-weight: bold; 
            width: 90%; margin-top: 0.8rem;
            transition: all 0.3s ease;
            box-shadow: 0 4px 6px rgba(52, 152, 219, 0.2);
            text-decoration: none; display: inline-block; box-sizing: border-box;
        }
        #submitBtn:hover { background: #2980b9; transform: translateY(-1px); }
        #submitBtn:disabled, #submitSmartBtn:disabled, #submitSmartXTCBtn:disabled { background: #95a5a6; cursor: not-allowed; transform: none; box-shadow: none; }
        #submitSmartBtn { background: #8e44ad; box-shadow: 0 4px 6px rgba(142, 68, 173, 0.2); }
        #submitSmartBtn:hover { background: #7d3c98; transform: translateY(-1px); }
        #submitSmartXTCBtn { background: #e67e22; box-shadow: 0 4px 6px rgba(230, 126, 34, 0.2); }
        #submitSmartXTCBtn:hover { background: #d35400; transform: translateY(-1px); }
        #downloadAllBtn { background: #27ae60; display: none; margin-top: 1.2rem; box-shadow: 0 4px 6px rgba(39, 174, 96, 0.2); }
        #downloadAllBtn:hover { background: #219653; transform: translateY(-1px); }
        
        #status { margin-top: 1.2rem; font-size: 0.95rem; text-align: left; background: #f8f9fa; padding: 12px 15px; border-radius: 8px; display: none;}
        .loading { color: #e67e22; font-weight: bold; }
        .success { color: #27ae60; font-weight: bold; }
        .error { color: #e74c3c; font-weight: bold; }
        .tips { font-size: 0.85rem; color: #7f8c8d; margin-top: 6px; border-top: 1px solid #eee; padding-top: 6px;}

        .task-list {
            margin-top: 1rem;
            text-align: left;
            max-height: 260px;
            overflow-y: auto;
            display: none;
            border: 1px solid #e2e8f0;
            border-radius: 8px;
            background: #ffffff;
            box-shadow: inset 0 2px 4px rgba(0,0,0,0.02);
        }
        .task-item {
            padding: 10px 14px;
            border-bottom: 1px solid #f1f5f9;
            display: flex;
            justify-content: space-between;
            align-items: center;
            font-size: 0.9rem;
        }
        .task-item:last-child { border-bottom: none; }
        .task-name {
            font-weight: 600;
            color: #334155;
            max-width: 220px;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
        }
        .task-status { font-size: 0.85rem; }
        .task-dl-btn {
            background: #27ae60; color: white; border: none;
            padding: 5px 12px; border-radius: 6px; text-decoration: none;
            font-size: 0.82rem; font-weight: bold; transition: background 0.2s;
        }
        .task-dl-btn:hover { background: #219653; }
        .orient-btn {
            background: #eef2f5; color: #333; border: 1px solid #ccc;
            padding: 5px 9px; border-radius: 6px; cursor: pointer;
            font-size: 0.85rem; font-weight: bold; transition: all 0.2s;
            white-space: nowrap;
        }
        .orient-btn.active {
            background: #e67e22; color: #fff; border-color: #d35400;
        }
    </style>
</head>
<body>
    <div class="card">
        <h2>📚 漫画全自动切边转换</h2>
        <form id="uploadForm">
            <div class="file-input-wrapper">
                <label for="file" class="upload-btn" id="fileLabel">点击选择 PDF/MOBI/AZW3 (支持多选) ...</label>
                <input type="file" id="file" name="file" accept=".pdf,.mobi,.azw3" multiple required>
            </div>
            <div style="margin-bottom:0.8rem; text-align:center;">
                <label style="font-size:0.9rem; font-weight:bold; color:#444; margin-right:8px;">目标设备 (Device):</label>
                <div style="display:inline-flex; gap:6px; flex-wrap:wrap; justify-content:center;">
                    <button type="button" id="deviceX4Btn" class="orient-btn active" onclick="setDevice('x4')">X4 (480x800)</button>
                    <button type="button" id="deviceX3Btn" class="orient-btn" onclick="setDevice('x3')">X3 (528x792)</button>
                    <button type="button" id="deviceEegoBtn" class="orient-btn" onclick="setDevice('eego')">eego (552x768)</button>
                </div>
            </div>
            <div style="margin-bottom:0.8rem; text-align:center;">
                <label style="font-size:0.9rem; font-weight:bold; color:#444; margin-right:8px;">导出屏幕方向 (Orientation):</label>
                <div style="display:inline-flex; gap:6px;">
                    <button type="button" id="orient0Btn" class="orient-btn" onclick="setOrientation(0)">0 (标准竖屏)</button>
                    <button type="button" id="orient90Btn" class="orient-btn active" onclick="setOrientation(90)">90 (旋转横屏)</button>
                </div>
            </div>
            <button type="button" id="submitBtn">上传转换（不建议使用）</button>
            <button type="button" id="submitSmartBtn">🔍 智能字体检测切分 (导出 PDF)</button>
            <button type="button" id="submitSmartXTCBtn">📱 智能切分并导出 XTC (旋转横屏 阅星曈X4 480x800)</button>
            <label style="display:inline-flex;align-items:center;gap:8px;margin-top:1rem;font-size:0.9rem;color:#555;cursor:pointer;">
                <input type="checkbox" id="keepOriginal" style="width:16px;height:16px;cursor:pointer;">
                保留切分前的原图
            </label>
        </form>
        <div id="status"></div>
        <div id="taskList" class="task-list"></div>
        <a id="downloadAllBtn" href="#">📦 一键打包下载全部文件 (ZIP)</a>
        <div style="margin-top: 1.5rem; padding-top: 1rem; border-top: 1px dashed #e0e0e0; font-size: 0.9rem;">
            🚀 觉得网页慢？<a href="https://asukalay-1253207553.cos.ap-chengdu.myqcloud.com/xteink.exe" style="color: #3498db; text-decoration: none; font-weight: bold;">点击下载 Windows 桌面版</a> (处理速度更快，支持整个文件夹)
        </div>
        <div style="margin-top: 0.6rem; font-size: 0.9rem; color: #e67e22;">
            ⚡ 文件大于 100M？<a href="http://203.88.127.184:8090/" target="_blank" style="color: #e67e22; text-decoration: underline; font-weight: bold;">请点击这里上传</a>
        </div>
    </div>
    <script>
        const fileInput = document.getElementById('file');
        const fileLabel = document.getElementById('fileLabel');
        const submitBtn = document.getElementById('submitBtn');
        const submitSmartBtn = document.getElementById('submitSmartBtn');
        const submitSmartXTCBtn = document.getElementById('submitSmartXTCBtn');
        const keepOriginal = document.getElementById('keepOriginal');
        const statusDiv = document.getElementById('status');
        const taskListDiv = document.getElementById('taskList');
        const downloadAllBtn = document.getElementById('downloadAllBtn');

        let currentOrientation = 90;
        let currentDevice = 'x4';

        function updateXtcBtnText() {
            const btn = document.getElementById('submitSmartXTCBtn');
            let dev = '阅星曈X4 480x800';
            if (currentDevice === 'x3') {
                dev = '阅星曈X3 528x792';
            } else if (currentDevice === 'eego' || currentDevice === 'eego_a4' || currentDevice === 'a4') {
                dev = 'eego 552x768';
            }
            btn.innerText = '📱 智能切分并导出 XTC (' + (currentOrientation === 90 ? '旋转横屏 ' : '标准竖屏 ') + dev + ')';
        }

        function setOrientation(val) {
            currentOrientation = val;
            document.getElementById('orient0Btn').className = 'orient-btn' + (val === 0 ? ' active' : '');
            document.getElementById('orient90Btn').className = 'orient-btn' + (val === 90 ? ' active' : '');
            updateXtcBtnText();
        }

        function setDevice(val) {
            currentDevice = val;
            document.getElementById('deviceX4Btn').className = 'orient-btn' + (val === 'x4' ? ' active' : '');
            document.getElementById('deviceX3Btn').className = 'orient-btn' + (val === 'x3' ? ' active' : '');
            const eegoBtn = document.getElementById('deviceEegoBtn');
            if (eegoBtn) {
                eegoBtn.className = 'orient-btn' + ((val === 'eego' || val === 'eego_a4' || val === 'a4') ? ' active' : '');
            }
            updateXtcBtnText();
        }

        let activeTasks = [];
        let activePollTimer = null;

        fileInput.addEventListener('change', (e) => {
            if(e.target.files.length > 0) {
                if (e.target.files.length === 1) {
                    fileLabel.innerText = '已选择: ' + e.target.files[0].name;
                } else {
                    fileLabel.innerText = '已选择 ' + e.target.files.length + ' 个文件';
                }
                fileLabel.style.borderColor = '#3498db';
                fileLabel.style.color = '#3498db';
                statusDiv.style.display = 'none';
                taskListDiv.style.display = 'none';
                downloadAllBtn.style.display = 'none';
            } else {
                fileLabel.innerText = '点击选择 PDF / MOBI / AZW3 (支持多选) ...';
                fileLabel.style.borderColor = '#bdc3c7';
                fileLabel.style.color = '#2c3e50';
            }
        });

        async function uploadTo(endpoint) {
            if(fileInput.files.length === 0) {
                statusDiv.style.display = 'block';
                statusDiv.innerHTML = '<span class="error">请先选择文件！</span>';
                return;
            }

            const files = Array.from(fileInput.files);
            submitBtn.disabled = true;
            submitSmartBtn.disabled = true;
            submitSmartXTCBtn.disabled = true;
            downloadAllBtn.style.display = 'none';
            statusDiv.style.display = 'block';
            statusDiv.innerHTML = '<span class="loading">⬆️ 正在上传文件至服务器...</span>';
            
            taskListDiv.style.display = 'block';
            taskListDiv.innerHTML = '';
            activeTasks = [];

            if (activePollTimer) clearInterval(activePollTimer);

            for (let i = 0; i < files.length; i++) {
                const file = files[i];
                const formData = new FormData();
                formData.append("file", file);
                formData.append("keep_original", keepOriginal.checked ? "1" : "0");
                formData.append("orientation", currentOrientation);
                formData.append("device", currentDevice);

                const rowId = 'task-row-' + i;
                taskListDiv.innerHTML += '<div class="task-item" id="' + rowId + '">' +
                    '<div class="task-name" title="' + file.name + '">📄 ' + file.name + '</div>' +
                    '<div class="task-status loading" id="status-' + i + '">⬆️ 上传中...</div>' +
                    '</div>';

                try {
                    const response = await fetch(endpoint, { method: 'POST', body: formData });
                    if(!response.ok) { throw new Error(await response.text()); }
                    const data = await response.json();
                    const taskId = data.task_id;

                    document.getElementById('status-' + i).innerText = '⏳ 处理中...';
                    activeTasks.push({
                        index: i,
                        taskId: taskId,
                        fileName: file.name,
                        endpoint: endpoint,
                        status: 'processing'
                    });
                } catch (err) {
                    const elem = document.getElementById('status-' + i);
                    if (elem) {
                        elem.className = 'task-status error';
                        elem.innerText = '❌ 上传失败';
                    }
                }
            }

            if (activeTasks.length > 0) {
                statusDiv.innerHTML = '<span class="loading">⏳ 服务器正在处理中 (0/' + activeTasks.length + ' 完成)...</span><div class="tips">处理完成后下方将出现【一键打包下载】按钮，也可单独下载各个文件。</div>';
                activePollTimer = setInterval(pollAllTasks, 3000);
            } else {
                statusDiv.innerHTML = '<span class="error">❌ 所有文件上传失败</span>';
                submitBtn.disabled = false;
                submitSmartBtn.disabled = false;
                submitSmartXTCBtn.disabled = false;
            }
        }

        async function pollAllTasks() {
            let doneCount = 0;
            let finishedCount = 0;

            for (let task of activeTasks) {
                if (task.status === 'done' || task.status === 'error') {
                    finishedCount++;
                    if (task.status === 'done') doneCount++;
                    continue;
                }

                try {
                    const res = await fetch('/status?task_id=' + task.taskId);
                    const data = await res.json();
                    
                    const statusElem = document.getElementById('status-' + task.index);

                    if (data.status === 'done') {
                        task.status = 'done';
                        doneCount++;
                        finishedCount++;
                        
                        let outName = task.fileName;
                        const lastDot = outName.lastIndexOf('.');
                        if(lastDot !== -1) { outName = outName.substring(0, lastDot); }
                        var isXTC = (data.out_file && data.out_file.endsWith('.xtc')) || task.endpoint === '/upload-smart-xtc';
                        outName += isXTC ? ".xtc" : "_output.pdf";

                        const dlUrl = '/download?task_id=' + task.taskId + '&filename=' + encodeURIComponent(outName);
                        if (statusElem) {
                            statusElem.className = 'task-status success';
                            let html = '<a class="task-dl-btn" href="' + dlUrl + '" target="_blank">📥 下载 ' + (isXTC ? 'XTC' : 'PDF') + '</a>';
                            if (isXTC) {
                                html += ' <button class="task-dl-btn" style="background:#8e44ad; cursor:pointer;" onclick="openXtcPreview(\'' + dlUrl + '\', \'' + outName + '\')">👁️ 预览</button>';
                            }
                            statusElem.innerHTML = html;
                        }
                    } else if (data.status === 'error') {
                        task.status = 'error';
                        finishedCount++;
                        if (statusElem) {
                            statusElem.className = 'task-status error';
                            statusElem.innerText = '❌ 转换失败';
                        }
                    }
                } catch (err) {
                    // 网络短路重试
                }
            }

            statusDiv.innerHTML = '<span class="loading">⏳ 转换处理中 (' + doneCount + '/' + activeTasks.length + ' 完成)...</span><div class="tips">由于运算量大，请耐心等待。网页无需刷新。</div>';

            if (finishedCount === activeTasks.length) {
                clearInterval(activePollTimer);
                submitBtn.disabled = false;
                submitSmartBtn.disabled = false;
                submitSmartXTCBtn.disabled = false;

                if (doneCount > 0) {
                    statusDiv.innerHTML = '<span class="success">🎉 转换完成！(' + doneCount + '/' + activeTasks.length + ' 成功)</span>';
                    
                    const completedTaskIds = activeTasks.filter(t => t.status === 'done').map(t => t.taskId);
                    downloadAllBtn.href = '/download-all?task_ids=' + completedTaskIds.join(',');
                    downloadAllBtn.style.display = 'inline-block';
                } else {
                    statusDiv.innerHTML = '<span class="error">❌ 所有任务转换失败</span>';
                }
            }
        }

        let currentXtcData = null;
        let currentXtcPage = 0;

        async function openXtcPreview(url, fileName) {
            const modal = document.getElementById('xtcModal');
            document.getElementById('modalTitle').innerText = '👁️ 在线预览: ' + fileName;
            modal.style.display = 'flex';

            try {
                const resp = await fetch(url);
                const buffer = await resp.arrayBuffer();
                parseXtcBuffer(buffer);
                renderXtcPage(0);
            } catch (err) {
                alert('读取 XTC 渲染失败: ' + err.message);
            }
        }

        function closeXtcModal() {
            document.getElementById('xtcModal').style.display = 'none';
        }

        function parseXtcBuffer(buffer) {
            const view = new DataView(buffer);
            const bytes = new Uint8Array(buffer);
            const pageCount = view.getUint16(6, true);
            const indexOffset = Number(view.getBigUint64(24, true));

            const pages = [];
            let idxPos = indexOffset;
            for (let i = 0; i < pageCount; i++) {
                const pageOffset = Number(view.getBigUint64(idxPos, true));
                const pageSize = view.getUint32(idxPos + 8, true);
                const width = view.getUint16(idxPos + 12, true);
                const height = view.getUint16(idxPos + 14, true);
                pages.push({ offset: pageOffset, size: pageSize, width: width, height: height });
                idxPos += 16;
            }

            currentXtcData = { bytes, pages, pageCount };
            currentXtcPage = 0;
        }

        function renderXtcPage(pageNum) {
            if (!currentXtcData || pageNum < 0 || pageNum >= currentXtcData.pageCount) return;
            currentXtcPage = pageNum;

            const page = currentXtcData.pages[pageNum];
            const canvas = document.getElementById('xtcCanvas');
            canvas.width = page.width;
            canvas.height = page.height;
            const ctx = canvas.getContext('2d');

            const imageData = ctx.createImageData(page.width, page.height);
            const data = imageData.data;
            const bytes = currentXtcData.bytes;

            const xtgHeaderSize = 22;
            const bitmapOffset = page.offset + xtgHeaderSize;
            const rowBytes = Math.ceil(page.width / 8);

            for (let y = 0; y < page.height; y++) {
                for (let x = 0; x < page.width; x++) {
                    const byteIdx = bitmapOffset + y * rowBytes + Math.floor(x / 8);
                    const bitIdx = 7 - (x % 8);
                    const isWhite = (bytes[byteIdx] & (1 << bitIdx)) !== 0;

                    const pxIdx = (y * page.width + x) * 4;
                    const val = isWhite ? 255 : 0;
                    data[pxIdx] = val;
                    data[pxIdx + 1] = val;
                    data[pxIdx + 2] = val;
                    data[pxIdx + 3] = 255;
                }
            }

            ctx.putImageData(imageData, 0, 0);

            document.getElementById('pageCounter').innerText = '页码: ' + (pageNum + 1) + ' / ' + currentXtcData.pageCount;
            document.getElementById('prevPageBtn').disabled = pageNum === 0;
            document.getElementById('nextPageBtn').disabled = pageNum >= currentXtcData.pageCount - 1;
        }

        function changeXtcPage(delta) {
            renderXtcPage(currentXtcPage + delta);
        }

        submitBtn.addEventListener('click', async () => { uploadTo('/upload'); });
        submitSmartBtn.addEventListener('click', async () => { uploadTo('/upload-smart'); });
        submitSmartXTCBtn.addEventListener('click', async () => { uploadTo('/upload-smart-xtc'); });
    </script>

    <!-- XTC 预览 Modal -->
    <div id="xtcModal" style="display:none; position:fixed; top:0; left:0; width:100vw; height:100vh; background:rgba(0,0,0,0.75); z-index:9999; justify-content:center; align-items:center;">
        <div style="background:#fff; padding:20px; border-radius:12px; max-width:90vw; max-height:90vh; text-align:center; position:relative; overflow:auto; box-shadow:0 10px 25px rgba(0,0,0,0.5);">
            <button onclick="closeXtcModal()" style="position:absolute; top:12px; right:16px; background:#e74c3c; color:#fff; border:none; border-radius:50%; width:32px; height:32px; font-weight:bold; cursor:pointer;">✕</button>
            <h3 id="modalTitle" style="margin-top:0; margin-bottom:12px; font-size:1.1rem; color:#2c3e50;">XTC 在线预览</h3>
            <div style="display:flex; justify-content:center; align-items:center; margin-bottom:12px;">
                <canvas id="xtcCanvas" style="border:1px solid #ccc; max-width:80vw; max-height:65vh; box-shadow:0 4px 10px rgba(0,0,0,0.15);"></canvas>
            </div>
            <div style="display:flex; justify-content:center; align-items:center; gap:16px;">
                <button id="prevPageBtn" onclick="changeXtcPage(-1)" style="padding:8px 18px; background:#3498db; color:#fff; border:none; border-radius:6px; font-weight:bold; cursor:pointer;">◀ 上一页</button>
                <span id="pageCounter" style="font-weight:bold; font-size:0.95rem; color:#444;">页码: 1 / 1</span>
                <button id="nextPageBtn" onclick="changeXtcPage(1)" style="padding:8px 18px; background:#3498db; color:#fff; border:none; border-radius:6px; font-weight:bold; cursor:pointer;">下一页 ▶</button>
            </div>
        </div>
    </div>
</body>
</html>
`

func getLocalIPs() []string {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				ips = append(ips, ipnet.IP.String())
			}
		}
	}
	return ips
}

func runWebServer(port string) {
	initWebTasksDir()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})

	http.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		err := r.ParseMultipartForm(500 << 20) // 最大 500MB
		if err != nil {
			http.Error(w, "文件太大或解析错误", http.StatusBadRequest)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "获取上传文件失败", http.StatusBadRequest)
			return
		}
		defer file.Close()

		ext := strings.ToLower(filepath.Ext(header.Filename))
		if ext != ".pdf" && ext != ".mobi" && ext != ".azw3" {
			http.Error(w, "不支持的文件格式，仅支持 pdf, mobi, azw3", http.StatusBadRequest)
			return
		}

		// 创建独立任务
		taskId := fmt.Sprintf("task_%d", time.Now().UnixNano())
		taskDir := filepath.Join(webTasksDir, taskId)
		os.MkdirAll(taskDir, 0755)

		inputPath := filepath.Join(taskDir, header.Filename)
		outPath := filepath.Join(taskDir, "output.pdf")

		inFile, err := os.Create(inputPath)
		if err != nil {
			http.Error(w, "内部错误：文件保存失败", http.StatusInternalServerError)
			return
		}
		io.Copy(inFile, file)
		inFile.Close()

		// 注册任务状态
		webTasksMutex.Lock()
		webTasks[taskId] = &WebTask{
			ID:        taskId,
			Status:    "processing",
			CreatedAt: time.Now(),
			FileName:  header.Filename,
			OutFile:   outPath,
		}
		webTasksMutex.Unlock()

		fmt.Printf("\n[Web] 接收到任务 %s: %s, 大小: %.2f MB\n", taskId, header.Filename, float64(header.Size)/(1024*1024))

		// 开启后台异步转换
		go func(id, in, out string) {
			err := processSingleFile(in, out)

			webTasksMutex.Lock()
			if task, ok := webTasks[id]; ok {
				if err != nil {
					task.Status = "error"
					task.ErrorMsg = err.Error()
					fmt.Printf("[Web] 任务 %s 失败: %v\n", id, err)
				} else {
					task.Status = "done"
					fmt.Printf("[Web] 任务 %s 成功完成！即将保留 20 分钟。\n", id)
					// 处理成功后可以删除原始文件减小硬盘占用
					os.Remove(in)
				}
			}
			webTasksMutex.Unlock()
		}(taskId, inputPath, outPath)

		// 立即返回任务ID
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"task_id": taskId})
	})

	http.HandleFunc("/upload-smart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		err := r.ParseMultipartForm(500 << 20)
		if err != nil {
			http.Error(w, "文件太大或解析错误", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "获取上传文件失败", http.StatusBadRequest)
			return
		}
		defer file.Close()
		keepOrig := r.FormValue("keep_original") == "1"
		ext := strings.ToLower(filepath.Ext(header.Filename))
		if ext != ".pdf" && ext != ".mobi" && ext != ".azw3" {
			http.Error(w, "不支持的文件格式，仅支持 pdf, mobi, azw3", http.StatusBadRequest)
			return
		}
		taskId := fmt.Sprintf("task_%d", time.Now().UnixNano())
		taskDir := filepath.Join(webTasksDir, taskId)
		os.MkdirAll(taskDir, 0755)
		inputPath := filepath.Join(taskDir, header.Filename)
		outPath := filepath.Join(taskDir, "output.pdf")
		inFile, err := os.Create(inputPath)
		if err != nil {
			http.Error(w, "内部错误：文件保存失败", http.StatusInternalServerError)
			return
		}
		io.Copy(inFile, file)
		inFile.Close()
		webTasksMutex.Lock()
		webTasks[taskId] = &WebTask{
			ID:        taskId,
			Status:    "processing",
			CreatedAt: time.Now(),
			FileName:  header.Filename,
			OutFile:   outPath,
		}
		webTasksMutex.Unlock()
		fmt.Printf("\n[Web-Smart] 接收到任务 %s: %s, 大小: %.2f MB\n", taskId, header.Filename, float64(header.Size)/(1024*1024))
		go func(id, in, out string, keepOrig bool) {
			err := processSmartSplitFile(in, out, keepOrig)
			webTasksMutex.Lock()
			if task, ok := webTasks[id]; ok {
				if err != nil {
					task.Status = "error"
					task.ErrorMsg = err.Error()
					fmt.Printf("[Web-Smart] 任务 %s 失败: %v\n", id, err)
				} else {
					task.Status = "done"
					fmt.Printf("[Web-Smart] 任务 %s 成功完成！\n", id)
					os.Remove(in)
				}
			}
			webTasksMutex.Unlock()
		}(taskId, inputPath, outPath, keepOrig)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"task_id": taskId})
	})

	http.HandleFunc("/upload-smart-xtc", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		err := r.ParseMultipartForm(500 << 20)
		if err != nil {
			http.Error(w, "文件太大或解析错误", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "获取上传文件失败", http.StatusBadRequest)
			return
		}
		defer file.Close()
		keepOrig := r.FormValue("keep_original") == "1"
		ext := strings.ToLower(filepath.Ext(header.Filename))
		if ext != ".pdf" && ext != ".mobi" && ext != ".azw3" {
			http.Error(w, "不支持的文件格式，仅支持 pdf, mobi, azw3", http.StatusBadRequest)
			return
		}
		taskId := fmt.Sprintf("task_%d", time.Now().UnixNano())
		taskDir := filepath.Join(webTasksDir, taskId)
		os.MkdirAll(taskDir, 0755)
		inputPath := filepath.Join(taskDir, header.Filename)
		outPath := filepath.Join(taskDir, "output.xtc")

		inFile, err := os.Create(inputPath)
		if err != nil {
			http.Error(w, "内部错误：文件保存失败", http.StatusInternalServerError)
			return
		}
		io.Copy(inFile, file)
		inFile.Close()

		webTasksMutex.Lock()
		webTasks[taskId] = &WebTask{
			ID:        taskId,
			Status:    "processing",
			CreatedAt: time.Now(),
			FileName:  header.Filename,
			OutFile:   outPath,
		}
		webTasksMutex.Unlock()

		orient, _ := strconv.Atoi(r.FormValue("orientation"))
		device := r.FormValue("device")
		if device == "" {
			device = "x4"
		}
		fmt.Printf("\n[Web-Smart-XTC] 接收到任务 %s: %s (Device=%s, Orient=%d), 大小: %.2f MB\n", taskId, header.Filename, device, orient, float64(header.Size)/(1024*1024))
		go func(id, in, out string, keepOrig bool, orient int, device string) {
			err := processSmartSplitToXTC(in, out, keepOrig, orient, device)
			webTasksMutex.Lock()
			if task, ok := webTasks[id]; ok {
				if err != nil {
					task.Status = "error"
					task.ErrorMsg = err.Error()
					fmt.Printf("[Web-Smart-XTC] 任务 %s 失败: %v\n", id, err)
				} else {
					task.Status = "done"
					fmt.Printf("[Web-Smart-XTC] 任务 %s 成功完成！导出 XTC 格式文件。\n", id)
					os.Remove(in)
				}
			}
			webTasksMutex.Unlock()
		}(taskId, inputPath, outPath, keepOrig, orient, device)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"task_id": taskId})
	})

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		taskId := r.URL.Query().Get("task_id")
		webTasksMutex.Lock()
		task, ok := webTasks[taskId]
		webTasksMutex.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if !ok {
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "error_msg": "任务不存在或已过期被清理"})
			return
		}

		json.NewEncoder(w).Encode(map[string]string{
			"status":    task.Status,
			"error_msg": task.ErrorMsg,
			"out_file":  filepath.Base(task.OutFile),
		})
	})

	http.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		taskId := r.URL.Query().Get("task_id")
		filename := r.URL.Query().Get("filename")

		webTasksMutex.Lock()
		task, ok := webTasks[taskId]
		webTasksMutex.Unlock()

		if !ok || task.Status != "done" {
			http.Error(w, "文件不存在或尚未完成处理", http.StatusNotFound)
			return
		}

		if filename == "" {
			filename = filepath.Base(task.OutFile)
		}

		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
		if strings.HasSuffix(strings.ToLower(task.OutFile), ".xtc") {
			w.Header().Set("Content-Type", "application/octet-stream")
		} else {
			w.Header().Set("Content-Type", "application/pdf")
		}
		http.ServeFile(w, r, task.OutFile)
	})

	http.HandleFunc("/download-all", func(w http.ResponseWriter, r *http.Request) {
		taskIdsStr := r.URL.Query().Get("task_ids")
		if taskIdsStr == "" {
			http.Error(w, "未指定任务列表", http.StatusBadRequest)
			return
		}
		ids := strings.Split(taskIdsStr, ",")

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=\"xteink_batch_converted.zip\"")

		zipWriter := zip.NewWriter(w)
		defer zipWriter.Close()

		for _, taskId := range ids {
			taskId = strings.TrimSpace(taskId)
			if taskId == "" {
				continue
			}

			webTasksMutex.Lock()
			task, ok := webTasks[taskId]
			webTasksMutex.Unlock()

			if !ok || task.Status != "done" {
				continue
			}

			fileToZip, err := os.Open(task.OutFile)
			if err != nil {
				continue
			}

			ext := filepath.Ext(task.FileName)
			baseName := strings.TrimSuffix(task.FileName, ext)
			outExt := filepath.Ext(task.OutFile)
			zipEntryName := baseName + "_output" + outExt

			wFile, err := zipWriter.Create(zipEntryName)
			if err != nil {
				fileToZip.Close()
				continue
			}

			io.Copy(wFile, fileToZip)
			fileToZip.Close()
		}
	})

	addr := ":" + port
	fmt.Printf("\n========== Web 服务已启动 ==========\n")
	fmt.Printf("请打开电脑或手机浏览器访问（需在同一 WiFi 下）:\n")
	fmt.Printf("➜  http://localhost:%s\n", port)
	for _, ip := range getLocalIPs() {
		fmt.Printf("➜  http://%s:%s\n", ip, port)
	}
	fmt.Printf("====================================\n\n")

	log.Fatal(http.ListenAndServe(addr, nil))
}
