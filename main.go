package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
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
	fmt.Printf("Step 2: 智能字体检测切分 (%d 张图片)...\n", len(files))

	outSeq := 0
	for _, f := range files {
		fext := strings.ToLower(filepath.Ext(f))
		if fext != ".jpg" && fext != ".png" && fext != ".jpeg" {
			continue
		}
		src, err := imaging.Open(f)
		if err != nil {
			continue
		}
		bounds := src.Bounds()
		w := bounds.Dx()
		h := bounds.Dy()
		if w == 0 {
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

const indexHTML = `
<!DOCTYPE html>
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
            min-height: 100vh; margin: 0; color: #333; padding-top: 10vh;
        }
        .card { 
            background: rgba(255, 255, 255, 0.95); 
            padding: 2.5rem 2rem; 
            border-radius: 16px; 
            box-shadow: 0 10px 30px rgba(0,0,0,0.1); 
            text-align: center; width: 90%; max-width: 450px; 
            backdrop-filter: blur(10px);
        }
        h2 { margin-top: 0; color: #2c3e50; font-size: 1.6rem; margin-bottom: 2rem; }
        .file-input-wrapper { margin: 1.5rem 0; }
        input[type="file"] { display: none; }
        .upload-btn { 
            background: #eef2f5; color: #2c3e50; 
            padding: 12px 24px; border-radius: 8px; 
            cursor: pointer; display: inline-block; 
            font-weight: 600; font-size: 1rem;
            border: 2px dashed #bdc3c7;
            transition: all 0.3s ease;
            width: 80%; box-sizing: border-box;
            word-break: break-all;
        }
        .upload-btn:hover { border-color: #3498db; color: #3498db; background: #f8faff; }
        #submitBtn, #submitSmartBtn, #downloadBtn {
            background: #3498db; color: white; border: none; 
            padding: 14px 24px; border-radius: 8px; 
            cursor: pointer; font-size: 1.1rem; font-weight: bold; 
            width: 80%; margin-top: 1rem;
            transition: background 0.3s ease;
            box-shadow: 0 4px 6px rgba(52, 152, 219, 0.2);
            text-decoration: none; display: inline-block; box-sizing: border-box;
        }
        #submitBtn:hover, #downloadBtn:hover { background: #2980b9; transform: translateY(-1px); }
        #submitBtn:disabled, #submitSmartBtn:disabled { background: #95a5a6; cursor: not-allowed; transform: none; box-shadow: none; }
        #submitSmartBtn { background: #8e44ad; box-shadow: 0 4px 6px rgba(142, 68, 173, 0.2); margin-top: 0.5rem; }
        #submitSmartBtn:hover { background: #7d3c98; transform: translateY(-1px); }
        #downloadBtn { background: #27ae60; display: none; margin-top: 1.5rem; box-shadow: 0 4px 6px rgba(39, 174, 96, 0.2); }
        #downloadBtn:hover { background: #219653; }
        #status { margin-top: 1.5rem; font-size: 0.95rem; min-height: 1.5rem; text-align: left; background: #f8f9fa; padding: 15px; border-radius: 8px; display: none;}
        .loading { color: #e67e22; font-weight: bold; }
        .success { color: #27ae60; font-weight: bold; }
        .error { color: #e74c3c; font-weight: bold; }
        .tips { font-size: 0.85rem; color: #7f8c8d; margin-top: 10px; border-top: 1px solid #eee; padding-top: 10px;}
    </style>
</head>
<body>
    <div class="card">
        <h2>📚 漫画全自动切边转换</h2>
        <form id="uploadForm">
            <div class="file-input-wrapper">
                <label for="file" class="upload-btn" id="fileLabel">点击选择 PDF/MOBI/AZW3 ...</label>
                <input type="file" id="file" name="file" accept=".pdf,.mobi,.azw3" required>
            </div>
            <button type="button" id="submitBtn">上传转换（不建议使用）</button>
            <button type="button" id="submitSmartBtn">🔍 智能字体检测切分</button>
            <label style="display:inline-flex;align-items:center;gap:8px;margin-top:1rem;font-size:0.9rem;color:#555;cursor:pointer;">
                <input type="checkbox" id="keepOriginal" style="width:16px;height:16px;cursor:pointer;">
                保留切分前的原图
            </label>
        </form>
        <div id="status"></div>
        <a id="downloadBtn" href="#" target="_blank">📥 下载转换后的 PDF</a>
    </div>
    <script>
        const fileInput = document.getElementById('file');
        const fileLabel = document.getElementById('fileLabel');
        const submitBtn = document.getElementById('submitBtn');
        const statusDiv = document.getElementById('status');
        const downloadBtn = document.getElementById('downloadBtn');
        let pollInterval;

        fileInput.addEventListener('change', (e) => {
            if(e.target.files.length > 0) {
                fileLabel.innerText = '已选择: ' + e.target.files[0].name;
                fileLabel.style.borderColor = '#3498db';
                fileLabel.style.color = '#3498db';
                statusDiv.style.display = 'none';
                downloadBtn.style.display = 'none';
            } else {
                fileLabel.innerText = '点击选择 PDF / MOBI / AZW3 ...';
                fileLabel.style.borderColor = '#bdc3c7';
                fileLabel.style.color = '#2c3e50';
            }
        });

        async function pollStatus(taskId, fileName) {
            try {
                const res = await fetch('/status?task_id=' + taskId);
                const data = await res.json();
                
                if (data.status === 'processing') {
                    statusDiv.innerHTML = '<span class="loading">⏳ 服务器正在疯狂切图合成中...</span><div class="tips">由于运算量大，根据页数可能需要 1~5 分钟。<br>您可以切到后台干别的事，网页不关就行。</div>';
                } else if (data.status === 'done') {
                    clearInterval(pollInterval);
                    statusDiv.innerHTML = '<span class="success">✅ 转换完成！请点击下方按钮下载。</span><div class="tips">为节省服务器空间，该文件将在 20 分钟后自动销毁，请尽快下载。</div>';
                    downloadBtn.style.display = 'inline-block';

                    let outName = fileName;
                    const lastDot = outName.lastIndexOf('.');
                    if(lastDot !== -1) { outName = outName.substring(0, lastDot); }
                    outName += "_output.pdf";

                    downloadBtn.href = '/download?task_id=' + taskId + '&filename=' + encodeURIComponent(outName);
                    submitBtn.disabled = false;
                    submitSmartBtn.disabled = false;
                } else if (data.status === 'error') {
                    clearInterval(pollInterval);
                    statusDiv.innerHTML = '<span class="error">❌ 转换失败: ' + data.error_msg + '</span>';
                    submitBtn.disabled = false;
                    submitSmartBtn.disabled = false;
                }
            } catch (err) {
                clearInterval(pollInterval);
                statusDiv.innerHTML = '<span class="error">❌ 获取状态失败，网络连接中断！</span>';
                submitBtn.disabled = false;
                submitSmartBtn.disabled = false;
            }
        }

        const submitSmartBtn = document.getElementById('submitSmartBtn');
        const keepOriginal = document.getElementById('keepOriginal');

        async function uploadTo(endpoint) {
            if(fileInput.files.length === 0) {
                statusDiv.style.display = 'block';
                statusDiv.innerHTML = '<span class="error">请先选择一个文件！</span>';
                return;
            }
            const file = fileInput.files[0];
            const formData = new FormData();
            formData.append("file", file);
            formData.append("keep_original", keepOriginal.checked ? "1" : "0");

            statusDiv.style.display = 'block';
            statusDiv.innerHTML = '<span class="loading">⬆️ 正在上传文件至服务器...</span>';
            submitBtn.disabled = true;
            submitSmartBtn.disabled = true;
            downloadBtn.style.display = 'none';

            try {
                const response = await fetch(endpoint, { method: 'POST', body: formData });
                if(!response.ok) { throw new Error(await response.text()); }
                const data = await response.json();
                const taskId = data.task_id;
                statusDiv.innerHTML = '<span class="loading">⏳ 文件已就绪，等待处理...</span>';
                pollInterval = setInterval(() => pollStatus(taskId, file.name), 3000);
            } catch (err) {
                statusDiv.innerHTML = '<span class="error">❌ 上传失败: ' + err.message + '</span>';
                submitBtn.disabled = false;
                submitSmartBtn.disabled = false;
            }
        }

        submitBtn.addEventListener('click', async () => { uploadTo('/upload'); });
        submitSmartBtn.addEventListener('click', async () => { uploadTo('/upload-smart'); });
    </script>
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
		})
	})

	http.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		taskId := r.URL.Query().Get("task_id")
		filename := r.URL.Query().Get("filename")
		if filename == "" {
			filename = "output.pdf"
		}

		webTasksMutex.Lock()
		task, ok := webTasks[taskId]
		webTasksMutex.Unlock()

		if !ok || task.Status != "done" {
			http.Error(w, "文件不存在或尚未完成处理", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
		w.Header().Set("Content-Type", "application/pdf")
		http.ServeFile(w, r, task.OutFile)
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
