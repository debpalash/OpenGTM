"""Build the deterministic GitHub/social card from the launch background."""

from pathlib import Path

from PIL import Image, ImageDraw, ImageFont


ROOT = Path(__file__).resolve().parents[1]
ASSETS = ROOT / "docs" / "assets"
INPUT = ASSETS / "opengtm-launch-background.png"
OUTPUT = ASSETS / "opengtm-social-preview.png"


def font(size: int, bold: bool = False) -> ImageFont.FreeTypeFont:
    windows_fonts = Path("C:/Windows/Fonts")
    name = "segoeuib.ttf" if bold else "segoeui.ttf"
    return ImageFont.truetype(str(windows_fonts / name), size)


def main() -> None:
    source = Image.open(INPUT).convert("RGB")
    ratio = 2.0
    crop_height = min(source.height, int(source.width / ratio))
    top = (source.height - crop_height) // 2
    image = source.crop((0, top, source.width, top + crop_height)).resize((1280, 640))

    # Preserve the generated pipeline art while guaranteeing readable copy.
    overlay = Image.new("RGBA", image.size, (0, 0, 0, 0))
    draw = ImageDraw.Draw(overlay)
    for x in range(760):
        alpha = int(184 * (1 - x / 760) ** 1.7)
        draw.line((x, 0, x, 640), fill=(5, 7, 13, alpha))
    image = Image.alpha_composite(image.convert("RGBA"), overlay)
    draw = ImageDraw.Draw(image)

    draw.rounded_rectangle((70, 66, 287, 108), radius=21, fill=(98, 104, 242, 54), outline=(151, 157, 255, 120), width=1)
    draw.ellipse((88, 82, 98, 92), fill="#20CFAF")
    draw.text((110, 76), "OPEN-SOURCE GTM", font=font(18, True), fill="#D9DBFF")
    draw.text((68, 154), "OpenGTM", font=font(74, True), fill="white", stroke_width=1)
    draw.text((70, 246), "Build pipeline.", font=font(44, True), fill="white")
    draw.text((70, 302), "Not busywork.", font=font(44, True), fill="#8FF0DE")
    draw.text((72, 384), "Source · enrich · research · act", font=font(24), fill="#C7CBD8")
    draw.text((72, 429), "Your infrastructure. Your keys. The bill before the run.", font=font(20), fill="#9299AA")
    draw.rounded_rectangle((70, 506, 375, 559), radius=12, fill="#FFFFFF")
    draw.text((94, 519), "github.com/debpalash/opengtm", font=font(18, True), fill="#11131A")

    image.convert("RGB").save(OUTPUT, quality=94, optimize=True)
    print(f"Wrote {OUTPUT}")


if __name__ == "__main__":
    main()
