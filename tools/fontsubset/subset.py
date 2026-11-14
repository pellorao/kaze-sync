#!/usr/bin/env python3
"""Rebuilds the Web UI fonts (internal/server/web/fonts/*.woff2, web/fonts.css and coverage/*.txt).

Usage: .tmp/fontvenv/bin/python tools/fontsubset/subset.py [downloads-dir]

Needs fonttools and brotli (uv venv .tmp/fontvenv; uv pip install --python .tmp/fontvenv/bin/python fonttools brotli). Sources (see
internal/server/web/fonts/SOURCE.md): Adobe Source Sans 3 (VF, renamed Kaze Sans), Adobe Source Han Sans (SubsetOTF, per region,
Regular and Bold), Adobe Source Serif 4 (VF pinned to weight 600, renamed Kaze Serif) and Source Han Serif (SubsetOTF, per region,
SemiBold) for the headings, JetBrains Mono 2.304. Every language's CJK face holds the common characters of its region
(a legacy national standard read through the Python codecs) plus every non-ASCII character of the Web UI texts.
The faces of Adobe's Source family carry the Reserved Font Name "Source" (OFL 1.1 clause 3), so every subset is shipped
under a Kaze name (see rename_family and rename_han).
"""
import io, os, re, sys, zipfile
from pathlib import Path
from fontTools.ttLib import TTFont
from fontTools import subset
from fontTools.varLib import instancer

REPO = Path(__file__).resolve().parents[2]
DL = Path(sys.argv[1] if len(sys.argv) > 1 else Path.home() / "projects/.downloads")
WEB = REPO / "internal/server/web"
OUT = WEB / "fonts"
COV = Path(__file__).resolve().parent / "coverage"

# Latin faces: these blocks plus every non-CJK character used by the UI texts.
LATIN_BLOCKS = [(0x20, 0x24F), (0x2B0, 0x2FF), (0x300, 0x36F), (0x370, 0x3FF), (0x400, 0x4FF), (0x1E00, 0x1EFF),
                (0x2000, 0x206F), (0x20A0, 0x20CF), (0x2100, 0x214F), (0x2190, 0x21FF), (0x2200, 0x22FF), (0x25A0, 0x25FF)]
# A CJK face declares these blocks; a character of them that the subset lacks falls to the next family of the stack.
CJK_RANGE = "U+1100-11FF,U+2E80-2FDF,U+3000-30FF,U+3130-318F,U+31F0-33FF,U+3400-4DBF,U+4E00-9FFF,U+A960-A97F,U+AC00-D7FF,U+F900-FAFF,U+FE30-FE4F,U+FF00-FFEF"
REGIONS = {"CN": "zh-CN", "TW": "zh-TW", "JP": "ja", "KR": "ko"}


CJK_BLOCKS = [(0x1100, 0x11FF), (0x2E80, 0x2FDF), (0x3000, 0x30FF), (0x3130, 0x318F), (0x31F0, 0x33FF), (0x3400, 0x4DBF),
              (0x4E00, 0x9FFF), (0xA960, 0xA97F), (0xAC00, 0xD7FF), (0xF900, 0xFAFF), (0xFE30, 0xFE4F), (0xFF00, 0xFFEF)]


def is_cjk(c):
    return any(a <= ord(c) <= b for a, b in CJK_BLOCKS)


def codec_chars(codec, spans):
    out = set()
    for lo, hi in spans:
        for hi_b in range(lo >> 8, (hi >> 8) + 1):
            for lo_b in range(0xA1, 0xFF) if codec != "big5" else list(range(0x40, 0x7F)) + list(range(0xA1, 0xFF)):
                code = (hi_b << 8) | lo_b
                if not lo <= code <= hi:
                    continue
                try:
                    out.add(bytes([hi_b, lo_b]).decode(codec))
                except UnicodeDecodeError:
                    pass
    return out


COMMON = {
    # GB 2312 symbols (rows 1-9) and level-1 hanzi (3755)
    "CN": lambda: codec_chars("gb2312", [(0xA1A1, 0xA9FE), (0xB0A1, 0xD7F9)]),
    # Big5 symbols and the 5401 most common traditional hanzi
    "TW": lambda: codec_chars("big5", [(0xA140, 0xA3BF), (0xA440, 0xC67E)]),
    # JIS X 0208 symbols, kana and level-1 kanji (2965)
    "JP": lambda: codec_chars("euc_jp", [(0xA1A1, 0xA8FE), (0xB0A1, 0xCFD3)]),
    # KS X 1001 symbols and the 2350 common hangul syllables
    "KR": lambda: codec_chars("euc_kr", [(0xA1A1, 0xACFE), (0xB0A1, 0xC8FE)]),
}


def text_chars(*paths):
    s = set()
    for p in paths:
        s |= {c for c in Path(p).read_text(encoding="utf-8") if ord(c) >= 0x80}
    return s


def lang_chars(lang):
    srv = REPO / "internal/server"
    files = [srv / f"i18n/{lang}.json", srv / "i18n.go", *sorted(WEB.glob("*.html")), WEB / "app.js"]
    return text_chars(*files)


def cmap_of(path_or_font):
    f = path_or_font if isinstance(path_or_font, TTFont) else TTFont(path_or_font)
    return set(f.getBestCmap())


def build(src_font, unicodes, dest, features=None):
    opts = subset.Options()
    opts.flavor = "woff2"
    opts.layout_features = features or ["kern", "liga", "locl", "mark", "mkmk", "ccmp", "vert", "vrt2", "case", "tnum", "zero"]  # no calt: JetBrains Mono ligatures would join characters of paths and glob rules
    opts.hinting = False
    opts.desubroutinize = True
    opts.name_IDs = [0, 1, 2, 3, 4, 5, 6, 13, 14]
    opts.notdef_outline = True
    sub = subset.Subsetter(opts)
    sub.populate(unicodes=sorted(unicodes))
    sub.subset(src_font)
    src_font.flavor = "woff2"
    src_font.save(dest)
    return dest.stat().st_size


SANS_FAMILY = "Kaze Sans"
SERIF_FAMILY = "Kaze Serif"
SERIF_WEIGHT = 600  # the only weight of the headings (h1, h2, h3, the breadcrumb title); 500 and 700 take this face too
SERIF_OPSZ = 20  # Source Serif 4's optical size axis default (the page title is 20 px, the card titles 16 px)


def rename_family(font, family=SANS_FAMILY, olds=("SourceSans3VF", "SourceSans3", "Source Sans 3")):
    """Gives a Modified Version of a Source face a name without the Reserved Font Name "Source" (OFL 1.1 clause 3).

    Every name record except the copyright (0), license (13) and license URL (14), which must stay, has the family
    name replaced; the face is shipped as "Kaze Sans" or "Kaze Serif". The original names stay in the copyright record.
    olds are the compact, then the spaced old names (the last one is the spaced form).
    """
    compact = family.replace(" ", "")
    for rec in font["name"].names:
        if rec.nameID in (0, 13, 14):
            continue
        text = rec.toUnicode()
        new = text
        for old in olds:
            new = new.replace(old, family if " " in old else compact)
        if rec.nameID in (1, 4, 16):
            new = family
        if new != text:
            rec.string = new
    return font


def rename_han(font, old, new):
    """The same for a Source Han face: "Source Han Sans CN" becomes "Kaze Han Sans CN" in every name record except 0, 13, 14.

    The localized family names (the Chinese or Japanese ones) are dropped: they name the original family.
    """
    names = font["name"]
    keep = []
    for rec in names.names:
        english = (rec.platformID == 3 and rec.langID == 0x409) or (rec.platformID == 1 and rec.langID == 0)
        if rec.nameID in (1, 3, 4, 6, 16, 17, 21, 25) and not english:
            continue
        keep.append(rec)
        if rec.nameID in (0, 13, 14):
            continue
        text = rec.toUnicode()
        out = text.replace(old, new).replace(old.replace(" ", ""), new.replace(" ", ""))
        if out != text:
            rec.string = out
    names.names = keep
    return font


def ranges(cps):
    cps = sorted(cps)
    out, lo, prev = [], None, None
    for c in cps:
        if lo is None:
            lo = prev = c
        elif c == prev + 1:
            prev = c
        else:
            out.append((lo, prev)); lo = prev = c
    if lo is not None:
        out.append((lo, prev))
    return ",".join("U+%04X" % a if a == b else "U+%04X-%04X" % (a, b) for a, b in out)


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    COV.mkdir(parents=True, exist_ok=True)
    all_text = set()
    for lang in REGIONS.values():
        all_text |= lang_chars(lang)
    all_text |= lang_chars("en")
    latin_want = {c for c in all_text if not is_cjk(c)} | {chr(c) for a, b in LATIN_BLOCKS for c in range(a, b + 1)}
    css = ["/* Generated by tools/fontsubset/subset.py; do not edit. */"]
    sizes = {}

    # Latin text: Source Sans 3 variable (weights 400-700 only)
    vf = TTFont(DL / "source-sans-3-vf/x/VF/SourceSans3VF-Upright.ttf")
    vf = instancer.instantiateVariableFont(vf, {"wght": (400, 700)})
    buf = io.BytesIO()
    vf.save(buf)  # reload: the instanced font keeps lazily loaded tables that the subsetter trips over
    vf = TTFont(io.BytesIO(buf.getvalue()))
    rename_family(vf)
    src_cm = cmap_of(vf)
    want = {ord(c) for c in latin_want} & src_cm
    sizes["sans"] = build(vf, want, OUT / "SourceSans3-Latin.woff2")
    got = cmap_of(OUT / "SourceSans3-Latin.woff2")
    (COV / "latin.txt").write_text("".join(chr(c) for c in sorted(got)) + "\n", encoding="utf-8")
    css.append('@font-face { font-family: "Kaze Sans"; font-style: normal; font-weight: 400 700; font-display: swap; src: url("/static/fonts/SourceSans3-Latin.woff2") format("woff2"); unicode-range: %s; }' % ranges(got))
    sans_src = src_cm

    # Headings, Latin: Source Serif 4 variable, pinned to one weight and the default optical size (one static face)
    sf = TTFont(DL / "source-serif-4/x/SourceSerif4Variable-Roman.ttf")
    sf = instancer.instantiateVariableFont(sf, {"wght": SERIF_WEIGHT, "opsz": SERIF_OPSZ})
    buf = io.BytesIO()
    sf.save(buf)
    sf = TTFont(io.BytesIO(buf.getvalue()))
    rename_family(sf, SERIF_FAMILY, olds=("SourceSerif4Variable", "SourceSerif4", "Source Serif 4 Variable", "Source Serif 4"))
    serif_src = cmap_of(sf)
    want = {ord(c) for c in latin_want} & serif_src
    sizes["serif"] = build(sf, want, OUT / "SourceSerif4-SemiBold-Latin.woff2")
    got = cmap_of(OUT / "SourceSerif4-SemiBold-Latin.woff2")
    (COV / "serif-latin.txt").write_text("".join(chr(c) for c in sorted(got)) + "\n", encoding="utf-8")
    css.append('@font-face { font-family: "Kaze Serif"; font-style: normal; font-weight: %d; font-display: swap; src: url("/static/fonts/SourceSerif4-SemiBold-Latin.woff2") format("woff2"); unicode-range: %s; }' % (SERIF_WEIGHT, ranges(got)))

    # Monospace: JetBrains Mono Regular and Bold
    z = zipfile.ZipFile(DL / "JetBrainsMono-2.304.zip")
    mono_src = set()
    for name, w in (("Regular", 400), ("Bold", 700)):
        f = TTFont(io.BytesIO(z.read(f"fonts/ttf/JetBrainsMono-{name}.ttf")))
        mono_src = cmap_of(f)
        want = {ord(c) for c in latin_want} & mono_src
        sizes["mono" + name] = build(f, want, OUT / f"JetBrainsMono-{name}-Latin.woff2")
        got = cmap_of(OUT / f"JetBrainsMono-{name}-Latin.woff2")
        css.append('@font-face { font-family: "JetBrains Mono"; font-style: normal; font-weight: %d; font-display: swap; src: url("/static/fonts/JetBrainsMono-%s-Latin.woff2") format("woff2"); unicode-range: %s; }' % (w, name, ranges(got)))
    (COV / "mono.txt").write_text("".join(chr(c) for c in sorted(got)) + "\n", encoding="utf-8")

    # CJK: Source Han Sans, one face pair per region
    nosource = set()
    for reg, lang in REGIONS.items():
        common = COMMON[reg]()
        for name, w in (("Regular", 400), ("Bold", 700)):
            f = TTFont(DL / f"source-han-sans-subset-otf/SourceHanSans{reg}-{name}.otf")
            cm = cmap_of(f)
            rename_han(f, "Source Han Sans", "Kaze Han Sans")
            # Han, kana and hangul, and any other character of the texts that Source Sans 3 lacks (such as the dots of the row menu)
            text = lang_chars(lang) | text_chars(REPO / "internal/server/i18n.go")
            want = {ord(c) for c in common | text if is_cjk(c)} | {ord(c) for c in text if ord(c) not in sans_src}
            want &= cm
            extra = sorted(c for c in want if not any(a <= c <= b for a, b in CJK_BLOCKS))
            dest = OUT / f"SourceHanSans{reg}-{name}-Subset.woff2"
            sizes[reg + name] = build(f, want, dest)
            css.append('@font-face { font-family: "Kaze Han Sans %s"; font-style: normal; font-weight: %d; font-display: swap; src: url("/static/fonts/%s") format("woff2"); unicode-range: %s; }' % (reg, w, dest.name, CJK_RANGE + ("," + ranges(extra) if extra else "")))
            if name == "Regular":
                got = cmap_of(dest)
                (COV / f"{reg.lower()}.txt").write_text("".join(chr(c) for c in sorted(got)) + "\n", encoding="utf-8")
                nosource |= {c for c in lang_chars(lang) if ord(c) not in cm and ord(c) not in sans_src}
    # CJK headings: Source Han Serif SemiBold, one face per region (the same character range as the sans faces)
    nosource_serif = set()
    for reg, lang in REGIONS.items():
        common = COMMON[reg]()
        f = TTFont(DL / f"source-han-serif-subset-otf/SourceHanSerif{reg}-SemiBold.otf")
        cm = cmap_of(f)
        rename_han(f, "Source Han Serif", "Kaze Han Serif")
        text = lang_chars(lang) | text_chars(REPO / "internal/server/i18n.go")
        want = {ord(c) for c in common | text if is_cjk(c)} | {ord(c) for c in text if ord(c) not in serif_src}
        want &= cm
        extra = sorted(c for c in want if not any(a <= c <= b for a, b in CJK_BLOCKS))
        dest = OUT / f"SourceHanSerif{reg}-SemiBold-Subset.woff2"
        sizes["Serif" + reg] = build(f, want, dest)
        css.append('@font-face { font-family: "Kaze Han Serif %s"; font-style: normal; font-weight: %d; font-display: swap; src: url("/static/fonts/%s") format("woff2"); unicode-range: %s; }' % (reg, SERIF_WEIGHT, dest.name, CJK_RANGE + ("," + ranges(extra) if extra else "")))
        (COV / f"serif-{reg.lower()}.txt").write_text("".join(chr(c) for c in sorted(cmap_of(dest))) + "\n", encoding="utf-8")
        nosource_serif |= {c for c in lang_chars(lang) if ord(c) not in cm and ord(c) not in serif_src}
    (COV / "nosource-serif.txt").write_text("".join(sorted(nosource_serif)) + "\n", encoding="utf-8")
    # The language names of the settings list: one tiny face per region, so the list does not pull the big subsets.
    names = dict(re.findall(r'\{"([A-Za-z-]+)", "([^"]+)"\}', (REPO / "internal/server/i18n.go").read_text(encoding="utf-8")))
    for reg, lang in REGIONS.items():
        f = TTFont(DL / f"source-han-sans-subset-otf/SourceHanSans{reg}-Regular.otf")
        rename_han(f, "Source Han Sans", "Kaze Han Sans")
        want = {ord(c) for c in names[lang]} & cmap_of(f)
        dest = OUT / f"SourceHanSans{reg}-Name.woff2"
        sizes["Name" + reg] = build(f, want, dest)
        got = cmap_of(dest)
        (COV / f"name-{reg.lower()}.txt").write_text("".join(chr(c) for c in sorted(got)) + "\n", encoding="utf-8")
        css.append('@font-face { font-family: "Kaze Name %s"; font-style: normal; font-weight: 400; font-display: swap; src: url("/static/fonts/%s") format("woff2"); unicode-range: %s; }' % (reg, dest.name, ranges(got)))
    (COV / "nosource.txt").write_text("".join(sorted(nosource)) + "\n", encoding="utf-8")
    (OUT.parent / "fonts.css").write_text("\n".join(css) + "\n")
    for k, v in sizes.items():
        print("%-14s %8d bytes" % (k, v))
    print("total", sum(sizes.values()))


if __name__ == "__main__":
    main()
