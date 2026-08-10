import argparse
import fitz  # PyMuPDF
from PIL import Image
import struct

def convert_pdf_to_xtc(pdf_path, xtc_path, width=400, height=300):
    doc = fitz.open(pdf_path)
    images = []
    print(f"正在解析 PDF，共 {len(doc)} 页...")
    for page in doc:
        pix = page.get_pixmap(dpi=150)
        img = Image.frombytes("RGB", [pix.width, pix.height], pix.samples)
        # 缩放至 400x300 并进行 1-bit 黑白抖动处理
        img = img.resize((width, height)).convert("L").convert("1")
        images.append(img)
    
    with open(xtc_path, "wb") as f:
        # XTC 基础帧头
        f.write(b"XTC\x00")
        f.write(struct.pack("<HHH", width, height, len(images)))
        for img in images:
            f.write(img.tobytes())
    print(f"成功导出 {len(images)} 页到 {xtc_path}")

if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("-i", "--input", required=True)
    parser.add_argument("-o", "--output", required=True)
    parser.add_argument("--width", type=int, default=400)
    parser.add_argument("--height", type=int, default=300)
    args = parser.parse_args()
    
    convert_pdf_to_xtc(args.input, args.output, args.width, args.height)