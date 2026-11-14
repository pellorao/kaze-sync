# Web UI fonts

All three families are licensed under the SIL Open Font License 1.1 (the license files sit beside the fonts). The
`.woff2` files are subsets built by `tools/fontsubset/subset.py` (see its header for how to rebuild them); the
characters each subset holds are listed in `tools/fontsubset/coverage/`.

| Files | Family | Source |
|---|---|---|
| `SourceSans3-Latin.woff2` | Source Sans 3 (variable, weights 400-700) | https://github.com/adobe-fonts/source-sans release 3.052R, `VF-source-sans-3.052R.zip` |
| `SourceHanSans{CN,TW,JP,KR}-{Regular,Bold}-Subset.woff2` | Source Han Sans (regional OTF: CN = Simplified Chinese, TW = Traditional Chinese, JP, KR) | https://github.com/adobe-fonts/source-han-sans branch `release`, `SubsetOTF/<region>/`, version 2.005R |
| `JetBrainsMono-{Regular,Bold}-Latin.woff2` | JetBrains Mono 2.304 | https://github.com/JetBrains/JetBrainsMono/releases/tag/v2.304 |

Source Han Sans has the same glyphs as Google's Noto Sans CJK. A Han, kana or hangul character outside a subset is set
in a Source Han Sans or Noto Sans CJK installed on the machine, then in a system font.
