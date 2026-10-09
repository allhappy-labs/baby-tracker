# App icon — "Cradle"

A crescent opening upward, holding a single drop: moon, cradle, and feed in one mark.
Palette is the app's Night Shift amber (`#F8D9A6` → `#EFA84E`) on deep ink
(`#1F2A50` → `#0A0E1C`).

## What's here

| File | Purpose |
|---|---|
| `appicon-composed.svg` | The full icon, background included. Source of truth. |
| `layers/1-cradle.svg` | Bottom layer, foreground only, transparent background. |
| `layers/2-drop.svg` | Top layer, foreground only, transparent background. |
| `layers-png/*.png` | Same two layers at 1024, in case Icon Composer dislikes the SVG mask. |

Layers are numbered by Z-index, which is the order Icon Composer applies them.

## Current state: asset catalog, not `.icon`

`Apps/BabyTrackerApp/Assets.xcassets/AppIcon.appiconset` carries a flattened 1024
PNG. This works on iOS 26 and is what ships today, but it gets the *flat* treatment —
no layered parallax, no specular pass, and no automatic light/dark/tinted/clear variants.

Icon Composer has no CLI and the `.icon` bundle format is undocumented, so it was not
hand-authored — guessing at an opaque binary format was not worth it when the asset
catalog path works and is verifiable.

## Upgrading to the layered `.icon` (about a minute, needs the GUI)

1. Open `/Applications/Xcode.app/Contents/Applications/Icon Composer.app`.
2. New document, 1024×1024.
3. Drag in `layers/1-cradle.svg` then `layers/2-drop.svg` (order matters).
4. Set the background gradient in Icon Composer rather than importing one — top
   `#1F2A50`, bottom `#0A0E1C`.
5. Check all four modes. **Tinted mode is the one to scrutinise**: it discards colour, so
   the mark has to survive as a silhouette. It does, but confirm the drop still reads as
   separate from the cradle after the specular pass fattens edges.
6. Export the `.icon`, drop it into `Apps/BabyTrackerApp/`, and point
   `ASSETCATALOG_COMPILER_APPICON_NAME` at it.

## Constraints worth remembering

- **No baked-in effects.** No blur, shadow, or glass in the source art — Icon Composer
  applies its own and pre-baked ones fight it.
- **No squircle.** The system masks the icon; draw to the full square canvas.
- **No alpha in the shipped PNG.** App Store review rejects icons with an alpha channel,
  even a fully opaque one. The committed PNG is flattened (`hasAlpha: no`).
