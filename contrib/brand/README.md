# robinmail brand

The mark is a robin in profile: a dark body, an orange breast, a cream eye. No text in the icon.

## Colours

| Name   | Hex       | Use                              |
|--------|-----------|----------------------------------|
| Cream  | `#f4efe6` | background of the icon, the eye  |
| Robin  | `#e8642c` | the breast, accents              |
| Ink    | `#2a211c` | the body and beak                |

## Files

| File                   | What                                         | Use for                                                       |
|------------------------|----------------------------------------------|---------------------------------------------------------------|
| `logo.svg`             | full colour, square, cream full bleed        | the source; anything that rounds or crops the image itself     |
| `logo.png`             | 512 px, square, opaque                       | GitHub organization and repository avatars, social profiles    |
| `logo-1024.png`        | 1024 px, square, opaque                      | app stores, large previews                                     |
| `logo-rounded.png`     | 512 px, rounded corners, transparent outside | READMEs, docs and slides where nothing rounds it for you       |
| `apple-touch-icon.png` | 180 px, square, opaque                       | iOS home screen (iOS rounds it)                                |
| `favicon.svg`          | rounded cream tile                           | browser tab icon of the webmail and the docs site              |
| `logo-mono.svg`        | one colour, `currentColor`                   | inline in UI, print, places that allow one colour only         |

Upload the square opaque PNGs to services that crop to a circle or rounded square (GitHub does).
A transparent or pre-rounded image there shows its corners in the page background.

Render PNGs with `rsvg-convert` (`brew install librsvg`), which keeps transparency:

```sh
rsvg-convert -w 512 -h 512 logo.svg -o logo.png
rsvg-convert -w 1024 -h 1024 logo.svg -o logo-1024.png
rsvg-convert -w 512 -h 512 favicon.svg -o logo-rounded.png
rsvg-convert -w 180 -h 180 logo.svg -o apple-touch-icon.png
```
