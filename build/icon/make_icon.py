from PIL import Image, ImageDraw, ImageFilter
from pathlib import Path

OUT = Path(__file__).resolve().parents[1] / "app" / "app.ico"
SIZE = 512

im = Image.new("RGBA", (SIZE, SIZE), (0,0,0,0))
px = im.load()
top = (10, 22, 38)
bottom = (4, 10, 18)
for y in range(SIZE):
    t = y / (SIZE-1)
    r = int(top[0]*(1-t)+bottom[0]*t)
    g = int(top[1]*(1-t)+bottom[1]*t)
    b = int(top[2]*(1-t)+bottom[2]*t)
    for x in range(SIZE):
        px[x,y] = (r,g,b,255)

d = ImageDraw.Draw(im)
# rounded-square background
d.rounded_rectangle((18,18,SIZE-18,SIZE-18), radius=96, outline=(55,120,190,155), width=8)
# subtle inner glow
glow = Image.new("RGBA", (SIZE,SIZE), (0,0,0,0))
gd = ImageDraw.Draw(glow)
gd.ellipse((85,45,430,390), fill=(30,145,255,75))
glow = glow.filter(ImageFilter.GaussianBlur(55))
im = Image.alpha_composite(im, glow)
d = ImageDraw.Draw(im)

# controller silhouette
body = [
    (118,192),(154,142),(206,128),(256,146),(306,128),(358,142),(394,192),
    (427,301),(395,347),(354,335),(319,285),(193,285),(158,335),(117,347),(85,301)
]
d.polygon(body, fill=(87,176,255,255))
d.rounded_rectangle((126,150,386,282), radius=70, fill=(87,176,255,255))

# controller highlight
d.rounded_rectangle((142,164,370,214), radius=30, fill=(142,211,255,110))

# D-pad
d.rounded_rectangle((151,194,213,220), radius=7, fill=(9,26,44,255))
d.rounded_rectangle((169,176,195,238), radius=7, fill=(9,26,44,255))

# buttons
for cx,cy in [(330,185),(355,210),(305,210),(330,235)]:
    d.ellipse((cx-13,cy-13,cx+13,cy+13), fill=(9,26,44,255))

# sticks
for cx,cy in [(221,247),(291,247)]:
    d.ellipse((cx-19,cy-19,cx+19,cy+19), fill=(11,34,56,255))
    d.ellipse((cx-9,cy-9,cx+9,cy+9), fill=(154,220,255,255))

# small EN badge
badge=(183,326,329,410)
d.rounded_rectangle(badge, radius=27, fill=(11,27,45,235), outline=(87,176,255,220), width=5)
try:
    from PIL import ImageFont
    font_paths = [
        r"C:\Windows\Fonts\seguisb.ttf",
        r"C:\Windows\Fonts\arialbd.ttf",
    ]
    font = None
    for p in font_paths:
        if Path(p).exists():
            font = ImageFont.truetype(p, 61)
            break
    if font is None:
        font = ImageFont.load_default()
    txt="EN"
    box=d.textbbox((0,0), txt, font=font)
    tw,th=box[2]-box[0],box[3]-box[1]
    d.text(((badge[0]+badge[2]-tw)/2,(badge[1]+badge[3]-th)/2-4),txt,font=font,fill=(235,248,255,255))
except Exception:
    pass

# downsample for cleaner edges
im = im.resize((256,256), Image.Resampling.LANCZOS)
OUT.parent.mkdir(parents=True, exist_ok=True)
im.save(OUT, format="ICO", sizes=[(256,256),(128,128),(64,64),(48,48),(32,32),(24,24),(16,16)])
print(OUT)
