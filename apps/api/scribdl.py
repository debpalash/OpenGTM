#!/usr/bin/env python3

from bs4 import BeautifulSoup
import img2pdf
import os
import requests
import shutil
import sys
import argparse
import re

IMAGES = []

IMAGES_DIR = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "../../data/scribd/images"
)
PDF_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "../../data/scribd")
DATA_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "../../data")
HEADERS = {
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"
}


def ensure_dirs():
    os.makedirs(IMAGES_DIR, exist_ok=True)
    os.makedirs(PDF_DIR, exist_ok=True)


def get_arguments():
    parser = argparse.ArgumentParser(
        description="Download documents or text from Scribd"
    )

    parser.add_argument(
        "url", metavar="URL", type=str, help="Scribd document URL to download"
    )
    parser.add_argument(
        "-i",
        "--images",
        help="Download document as images",
        action="store_true",
        default=False,
    )

    return parser.parse_args()


def fix_encoding(query):
    if sys.version_info > (3, 0):
        return query
    else:
        return query.encode("utf-8")


def get_total_pages(url):
    response = requests.get(url, headers=HEADERS).text
    soup = BeautifulSoup(response, "html.parser")
    span = soup.find("span", {"data-e2e": "total-pages"})
    if span:
        total_pages = span.get_text().replace("/", "").strip()
        return int(total_pages)
    return None


def save_image(content, page_num, found=False):
    """
    Download an image and save it in IMAGES_DIR.
    Overwrites existing images if present.
    """
    global IMAGES
    ensure_dirs()
    image_path = os.path.join(IMAGES_DIR, f"{page_num}.jpg")

    if os.path.exists(image_path):
        os.remove(image_path)

    if content.endswith(".jsonp"):
        replacement = content.replace("/pages/", "/images/")
        if found:
            replacement = replacement.replace(".jsonp", "/000.jpg")
        else:
            replacement = replacement.replace(".jsonp", ".jpg")
    else:
        replacement = content

    try:
        response = requests.get(replacement, headers=HEADERS, stream=True, timeout=15)
        if response.status_code == 200 and "image" in response.headers.get(
            "Content-Type", ""
        ):
            with open(image_path, "wb") as out_file:
                shutil.copyfileobj(response.raw, out_file)
            IMAGES.append(image_path)
        else:
            print(
                f"Failed to download image {page_num}: Status {response.status_code}, Type {response.headers.get('Content-Type')}"
            )
    except Exception as e:
        print(f"Error downloading image {page_num}: {e}")
    print(f"Downloaded image {page_num}/{TOTAL_PAGES}")


def save_text(jsonp, filename):
    """Extract text from .jsonp and append to a text file."""
    response = requests.get(jsonp, headers=HEADERS).text
    page_no = response[11:12]
    response_head = (
        response.replace(f'window.page{page_no}_callback(["', "")
        .replace("\\n", "")
        .replace("\\", "")
        .replace('"]);', "")
    )
    soup_content = BeautifulSoup(response_head, "html.parser")

    with open(filename, "a", encoding="utf-8") as feed:
        for x in soup_content.find_all("span", {"class": "a"}):
            feed.write(f"{fix_encoding(x.get_text())}\n")


def save_content(content, images, page_num, title, found=False):
    """Save either image or text content."""
    if content:
        if images:
            save_image(content, page_num, found)
        else:
            save_text(content, os.path.join(DATA_DIR, f"{title}.txt"))
        page_num += 1
    return page_num


def sanitize_title(title):
    forbidden_chars = r" *\"/\\<>:|(),"
    for ch in forbidden_chars:
        title = title.replace(ch, "_")
    return title


def convert_to_pdf(title):
    """Convert images in IMAGES_DIR to PDF in PDF_DIR."""
    ensure_dirs()
    if not IMAGES:
        return

    sorted_images = sorted(
        IMAGES, key=lambda x: int(os.path.splitext(os.path.basename(x))[0])
    )
    pdf_path = os.path.join(PDF_DIR, f"{title}.pdf")
    with open(pdf_path, "wb") as f:
        f.write(img2pdf.convert(sorted_images))


def get_scribd_document(url, images=False, status_callback=None):
    """
    Download Scribd document as images or text and convert images to PDF.
    Returns the path to the generated file.
    """
    if status_callback:
        status_callback(f"Fetching {url}...")

    response = requests.get(url, headers=HEADERS).text
    global TOTAL_PAGES
    TOTAL_PAGES = get_total_pages(url)

    if TOTAL_PAGES:
        if status_callback:
            status_callback(f"Found {TOTAL_PAGES} pages.")

    soup = BeautifulSoup(response, "html.parser")

    title = sanitize_title(soup.find("title").get_text())

    # Ensure title is not empty or just whitespace
    if not title or title.strip() == "":
        title = "scribd_document"

    page_num = 1
    if images:
        absimg = soup.find_all("img", {"class": "absimg"}, src=True)
        for img in absimg:
            save_image(img["src"], page_num)
            if status_callback:
                status_callback(f"Downloaded image {page_num}")
            page_num += 1

    js_text = soup.find_all("script", type="text/javascript")
    valid_jsonp_found = False
    for opening in js_text:
        script_content = opening.string
        if not script_content:
            continue
        matches = re.findall(r"https://.*?\.jsonp", script_content)
        for jsonp in matches:
            valid_jsonp_found = True
            page_num = save_content(jsonp, images, page_num, title)
            if not images and status_callback and page_num % 5 == 0:
                status_callback(f"Processed {page_num} text segments...")

    if not valid_jsonp_found and not images:
        if status_callback:
            status_callback(
                "Warning: No content found directly. Authentication might be required for full access."
            )

    if images:
        convert_to_pdf(title)
        output_path = os.path.join(PDF_DIR, f"{title}.pdf")
        if status_callback:
            status_callback(f"Created PDF: {output_path}")
        return output_path
    else:
        output_path = os.path.join(DATA_DIR, f"{title}.txt")
        if status_callback:
            status_callback(f"Saved text to: {output_path}")
        print(f"OUTPUT_FILE: {output_path}")  # Keep for CLI usage if needed
        return output_path


def command_line():
    args = get_arguments()
    get_scribd_document(args.url, images=args.images)


if __name__ == "__main__":
    command_line()
