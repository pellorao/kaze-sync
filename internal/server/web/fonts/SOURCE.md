# Web UI fonts

All the families are licensed under the SIL Open Font License 1.1 (the license files sit beside the fonts). The
`.woff2` files are subsets built by `tools/fontsubset/subset.py` (see its header for how to rebuild them); the
characters each subset holds are listed in `tools/fontsubset/coverage/`.

| Files | Family | Source |
|---|---|---|
| `SourceSans3-Latin.woff2` | Source Sans 3 (variable, weights 400-700), shipped as "Kaze Sans" | https://github.com/adobe-fonts/source-sans release 3.052R, `VF-source-sans-3.052R.zip` |
| `SourceHanSans{CN,TW,JP,KR}-{Regular,Bold}-Subset.woff2` | Source Han Sans (regional OTF: CN = Simplified Chinese, TW = Traditional Chinese, JP, KR) | https://github.com/adobe-fonts/source-han-sans branch `release`, `SubsetOTF/<region>/`, version 2.005R |
| `SourceSerif4-SemiBold-Latin.woff2` | Source Serif 4 (variable, pinned to weight 600 and optical size 20), shipped as "Kaze Serif"; the headings | https://github.com/adobe-fonts/source-serif release 4.005R, `source-serif-4.005_Desktop.zip`, `VAR/SourceSerif4Variable-Roman.ttf` |
| `SourceHanSerif{CN,TW,JP,KR}-SemiBold-Subset.woff2` | Source Han Serif (regional OTF, SemiBold), shipped as "Kaze Han Serif CN" etc.; the headings | https://github.com/adobe-fonts/source-han-serif branch `release`, `SubsetOTF/<region>/SourceHanSerif<region>-SemiBold.otf` |
| `JetBrainsMono-{Regular,Bold}-Latin.woff2` | JetBrains Mono 2.304 | https://github.com/JetBrains/JetBrainsMono/releases/tag/v2.304 |

Source Han Sans has the same glyphs as Google's Noto Sans CJK, and Source Han Serif those of Noto Serif CJK. A Han, kana or
hangul character outside a subset is set in a Source Han (or Noto CJK) face of the same kind installed on the machine, then in a
system font. The serif faces hold one weight (600) for the headings; the Han subsets have the same character range as the sans ones.

The Adobe Source faces (Source Sans 3, Source Serif 4, Source Han Sans, Source Han Serif) carry the Reserved Font Name "Source"
(see `LICENSE-source-sans.md`, `LICENSE-source-serif.md`, `LICENSE-source-han-sans.txt`, `LICENSE-source-han-serif.txt`). Our
subsets are Modified Versions, so `tools/fontsubset/subset.py` renames every family in the font's name table (`rename_family`
for the Latin faces: "Kaze Sans", "Kaze Serif"; `rename_han` for the Han faces: "Kaze Han Sans CN", "Kaze Han Serif CN" and so on),
and `fonts.css` and `style.css` use those names. The copyright, license and license URL records stay in the file. JetBrains Mono
has no Reserved Font Name.
