package main

import (
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
	inDir := flag.String("in", "", "输入目录 (留空则启动 Web 界面)")
	outDir := flag.String("out", ".", "输出目录 (命令行模式用)")
	port := flag.String("port", "8080", "Web 服务端口")
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
            display: flex; justify-content: center; align-items: center; 
            height: 100vh; margin: 0; color: #333;
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
        #submitBtn { 
            background: #3498db; color: white; border: none; 
            padding: 14px 24px; border-radius: 8px; 
            cursor: pointer; font-size: 1.1rem; font-weight: bold; 
            width: 80%; margin-top: 1rem;
            transition: background 0.3s ease;
            box-shadow: 0 4px 6px rgba(52, 152, 219, 0.2);
        }
        #submitBtn:hover { background: #2980b9; transform: translateY(-1px); }
        #submitBtn:disabled { background: #95a5a6; cursor: not-allowed; transform: none; box-shadow: none; }
        #status { margin-top: 1.5rem; font-size: 0.95rem; min-height: 1.5rem; text-align: left; background: #f8f9fa; padding: 10px; border-radius: 8px; display: none;}
        .loading { color: #e67e22; font-weight: bold; }
        .success { color: #27ae60; font-weight: bold; }
        .error { color: #e74c3c; font-weight: bold; }
    </style>
</head>
<body>
    <div class="card">
        <h2>📚 漫画自动切白边与转换</h2>
        <form id="uploadForm">
            <div class="file-input-wrapper">
                <label for="file" class="upload-btn" id="fileLabel">点击选择 PDF / MOBI / AZW3 ...</label>
                <input type="file" id="file" name="file" accept=".pdf,.mobi,.azw3" required>
            </div>
            <button type="button" id="submitBtn">上传并开始转换</button>
        </form>
        <div id="status"></div>
    </div>
    <script>
        const fileInput = document.getElementById('file');
        const fileLabel = document.getElementById('fileLabel');
        const submitBtn = document.getElementById('submitBtn');
        const statusDiv = document.getElementById('status');

        fileInput.addEventListener('change', (e) => {
            if(e.target.files.length > 0) {
                fileLabel.innerText = '已选择: ' + e.target.files[0].name;
                fileLabel.style.borderColor = '#3498db';
                fileLabel.style.color = '#3498db';
                statusDiv.style.display = 'none';
            } else {
                fileLabel.innerText = '点击选择 PDF / MOBI / AZW3 ...';
                fileLabel.style.borderColor = '#bdc3c7';
                fileLabel.style.color = '#2c3e50';
            }
        });

        submitBtn.addEventListener('click', async () => {
            if(fileInput.files.length === 0) {
                statusDiv.style.display = 'block';
                statusDiv.innerHTML = '<span class="error">请先选择一个文件！</span>';
                return;
            }
            const file = fileInput.files[0];
            const formData = new FormData();
            formData.append("file", file);

            statusDiv.style.display = 'block';
            statusDiv.innerHTML = '<span class="loading">正在上传并执行切图中...<br>这可能需要几分钟的时间，请耐心等待浏览器提示下载，不要关闭当前页面。</span>';
            submitBtn.disabled = true;

            try {
                const response = await fetch('/upload', {
                    method: 'POST',
                    body: formData
                });
                
                if(!response.ok) {
                    throw new Error(await response.text());
                }

                // 下载文件
                const blob = await response.blob();
                const url = window.URL.createObjectURL(blob);
                const a = document.createElement('a');
                a.style.display = 'none';
                a.href = url;
                
                // 构造新文件名
                let outName = file.name;
                const lastDot = outName.lastIndexOf('.');
                if(lastDot !== -1) {
                    outName = outName.substring(0, lastDot);
                }
                a.download = outName + "_output.pdf";
                
                document.body.appendChild(a);
                a.click();
                setTimeout(() => {
                    window.URL.revokeObjectURL(url);
                    document.body.removeChild(a);
                }, 100);
                
                statusDiv.innerHTML = '<span class="success">✅ 转换完成！已触发下载。</span>';
            } catch (err) {
                statusDiv.innerHTML = '<span class="error">❌ 处理失败: ' + err.message + '</span>';
            } finally {
                submitBtn.disabled = false;
            }
        });
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

		// 创建临时处理目录
		tempDir, err := os.MkdirTemp("", "web_upload_*")
		if err != nil {
			http.Error(w, "内部错误：无法创建临时目录", http.StatusInternalServerError)
			return
		}
		defer os.RemoveAll(tempDir) // 请求结束自动清理上传的原始文件和生成的 PDF

		inputPath := filepath.Join(tempDir, header.Filename)
		outPath := filepath.Join(tempDir, "output.pdf")

		inFile, err := os.Create(inputPath)
		if err != nil {
			http.Error(w, "内部错误：文件保存失败", http.StatusInternalServerError)
			return
		}
		io.Copy(inFile, file)
		inFile.Close()

		fmt.Printf("\n[Web 上传] 接收到文件: %s, 大小: %.2f MB\n", header.Filename, float64(header.Size)/(1024*1024))

		// 转换处理
		err = processSingleFile(inputPath, outPath)
		if err != nil {
			fmt.Printf("[Web 处理错误] %s 转换失败: %v\n", header.Filename, err)
			http.Error(w, "处理失败: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// 返回生成后的最新文件
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s_output.pdf\"", strings.TrimSuffix(header.Filename, ext)))
		w.Header().Set("Content-Type", "application/pdf")
		http.ServeFile(w, r, outPath)
		fmt.Printf("[Web 处理成功] 文件已返回给客户端 (临时文件即将自动清理)\n")
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
