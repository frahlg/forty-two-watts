---
"ftw": patch
---

The app header is no longer dark in light theme. Eleven base tokens — `--bg`, `--surface`, `--surface2`, `--border`, `--text`, `--text-dim` and the five named colours — predate the oklch palette and were declared only for dark mode, so they kept their dark values when the theme switched. `--surface` is `#1a1a2e`, which is what painted the header and the History disclosures a dark blue-violet on a light page. Each token now maps to its equivalent in the current palette, so they follow the theme and cannot drift from it. Dark mode is untouched.
