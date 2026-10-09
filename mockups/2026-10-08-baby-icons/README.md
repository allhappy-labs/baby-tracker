# Baby icons — redraw v3 (2026-10-08)

Status: approved 2026-10-08 (redraw v3)

The first drafts were rejected ("boy or girl icons are bad"). v2 was a full
redraw. v3 applies an independent review of v2: no partial opacity, clearer
pairs within each sex, and a reworked bun, flower and pacifier.

- `preview@3x.png` is an honest iPhone-scale render (headless Chrome,
  `--force-device-scale-factor=3`, 700×900 window). The 32 pt / 28 pt icons in
  it are the real 96 / 84 physical pixels.
- `preview.png` is the same page at 1×.
- `index.html` shows every icon at 96 px, 32 pt and 28 pt. It also has header mocks
  (`[icon] Name ⌄`, 32 pt icon + bold 22 pt name) for twins Max/Leo
  (boy.1/boy.2), Sophia/Mia (girl.1/girl.2) and Mira (neutral), a list-row
  mock at 28 pt for all nine, and the raw untinted files via `<img>`.
  The tinted icons are the shipped files `svg/<id>.svg` applied as CSS alpha
  masks, which is exactly how a template image is tinted. Nothing is drawn inline.
- `svg/<id>.svg` are the 9 standalone files to ship. Each uses viewBox `0 0 24 24` and only
  `circle`/`ellipse`/`path`. There is no `<use>`, `<symbol>`, CSS, gradient, text,
  transform or partial opacity. All art is `#000`, so only alpha matters.
- `build.py` generates `svg/` and `index.html`. `render.sh` rebuilds and
  re-renders both PNGs. It needs `--allow-file-access-from-files` so Chrome will
  load the SVG masks from `file://`.

## Design

- **Mixed weight.** The head outline is a 2.2 stroke on r 7.9, so its outer edge spans
  3.0–21.0 × 4.6–22.6. Strokes are 2.2 for the head and boy.1's curl and 1.8 for
  secondary details (smile, girl.3 band, neutral wisps). The one exception is boy.4's
  pacifier ring at 1.5, which keeps its hole open at 28 pt. Every identity
  feature is a **filled** silhouette.
- **Baby proportions.** Big round head, eyes set low (y 13.9), small filled
  ears and a small smile. All nine share the same filled oval eyes. There are no cheeks: the v2
  translucent cheeks read as bruises on the dark ground, and opaque blush
  dashes read as whiskers at 28 pt.
- **Sex signal = hair/accessory only.** Every icon has one; none is plain.
- **Pairs.** All 6 same-sex pairs in each set differ in silhouette *and* hue.
  The twin pairs (boy.1/boy.2, girl.1/girl.2) are far apart on both.

| id | feature | silhouette at 28 pt |
|---|---|---|
| boy.1 | single baby curl | open hook rising from the crown |
| boy.2 | spiky tousled fringe | solid hair cap, spiky top, zig-zag fringe |
| boy.3 | baseball cap | solid dome + button, brim out to the right (glyph shifted −0.6 x for balance) |
| boy.4 | pacifier | small oval shield over the mouth, ring hanging in an opened chin; no hair |
| girl.1 | big bow | tilted bow on the top-right of the head |
| girl.2 | pigtails | parted fringe + two teardrop bunches out to the sides, ears under them |
| girl.3 | headband + flower | band across the forehead, 5-petal flower with a centre hole, top-left |
| girl.4 | top-knot bun | parted fringe + big bun (r 2.9) sitting on the crown with a hair-tie pinch |
| neutral | newborn wisps | three short hair strands, muted tint |

## Tints (on `#0D1224`)

| id | hex |
|---|---|
| boy.1 | `#6FB2FF` |
| boy.2 | `#4FD8B0` |
| boy.3 | `#FFD84D` |
| boy.4 | `#8A84FF` |
| girl.1 | `#FF86B8` |
| girl.2 | `#FFB35C` |
| girl.3 | `#D89BFF` |
| girl.4 | `#FF6B6B` |
| neutral | `#9AA4C9` |

Neutral keeps the app's secondary text colour `#9AA4C9`.

## Iterations

1. **v2.0:** new base and filled features. At 28 pt the closed spiral curl
   read as a ball, the ring-in-shield pacifier read as a pig snout, the centred bow
   read as cat ears and the high puffs read as bear ears.
2. **v2.1:** the curl became an open hook. The bow moved to the top-right and tilted. Pigtails
   became parted hair plus teardrop bunches. The shield and ring were separated, but the
   result read as a moustache.
3. **v2.2:** the chin was opened for the pacifier ring. The flower was enlarged and moved outward.
   The bow clipping the viewBox was fixed. Neutral got three strands instead of a curl.
4. **v3.0 (review fixes):**
   - Removed the 0.35-opacity cheeks, which read as bruises. Tried opaque 1.2-wide blush dashes, but at 28 pt they read as
     whiskers or a widened mouth, so the set has no blush.
   - Removed boy.4's curl. The pacifier carries it alone, and the tint deepened to `#8A84FF`.
   - Moved boy.3 to yellow `#FFD84D`, so it is no longer a second green mass next to boy.2.
   - Moved girl.4 to red-coral `#FF6B6B`, away from girl.2 apricot and still distinct from girl.1 pink.
   - girl.4 bun: bigger (r 2.9) and sitting on the crown with no gap. It has a filled
     hair-tie pinch and a parted fringe. The fringe from the review's sketch, ending at y ≈ 8, sat
     entirely inside the head stroke and was invisible. It now reaches y ≈ 10.3 at the
     temples so it shows. girl.4 now reads as a girl with a top-knot, not a pompom.
   - girl.3 flower: one compound path. Petals are r 1.3, spread 0.3 further (2.5), around a core
     with an r 0.8 hole. It uses nonzero winding with the hole drawn anticlockwise rather than
     evenodd, because evenodd would punch holes where the core overlaps the petals.
   - girl.2: puff centres pulled in by 0.5 and ears restored.
   - boy.3: shifted the whole glyph −0.6 x instead of shortening the brim, which would
     barely clear the head and stop reading as a brim.
   - Standardised strokes (2.2 / 1.8).
   - boy.4 shield shrunk to rx 2.4 / ry 1.1 with a 0.5 gap above the ring. The ring moved down
     to cy 20.7 so the shield clears the eyes.

## Final SVGs (verbatim from `svg/`)

### boy.1 — Curl — tint `#6FB2FF`
```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="13.6" r="7.9" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="2.2"/><path d="M3.9 11.9a1.75 1.75 0 0 0 0 3.5z" fill="#000"/><path d="M20.1 11.9a1.75 1.75 0 0 1 0 3.5z" fill="#000"/><ellipse cx="9.1" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><ellipse cx="14.9" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><path d="M10.5 17.2q1.5 1.25 3 0" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8"/><path d="M11.7 5.4C10.9 3.2 12.2 1.3 14.3 1.3c1.7 0 2.6 1.5 1.9 2.9" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="2.2"/></svg>
```

### boy.2 — Spiky fringe — tint `#4FD8B0`
```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="13.6" r="7.9" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="2.2"/><path d="M3.9 11.9a1.75 1.75 0 0 0 0 3.5z" fill="#000"/><path d="M20.1 11.9a1.75 1.75 0 0 1 0 3.5z" fill="#000"/><ellipse cx="9.1" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><ellipse cx="14.9" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><path d="M10.5 17.2q1.5 1.25 3 0" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8"/><path d="M3.73 11.23 4.07 8.56 6.35 7.12 8.08 5.06 10.77 5.09 13.34 4.3 15.59 5.78 18.18 6.52 19.26 8.99 20.27 11.23 L17.8 10.6 16.4 8.6 14.8 10.1 13.2 8.3 11.6 9.9 10 8.2 8.4 9.8 7 8.4 6 10.4Z" fill="#000" stroke="#000" stroke-width=".6" stroke-linejoin="round"/></svg>
```

### boy.3 — Cap with brim — tint `#FFD84D`
```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="11.4" cy="13.6" r="7.9" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="2.2"/><path d="M3.3 11.9a1.75 1.75 0 0 0 0 3.5z" fill="#000"/><path d="M19.5 11.9a1.75 1.75 0 0 1 0 3.5z" fill="#000"/><ellipse cx="8.5" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><ellipse cx="14.3" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><path d="M9.9 17.2q1.5 1.25 3 0" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8"/><path d="M3.3 10.2C3.7 5.6 7 3.3 11.4 3.3s7.7 2.3 8.1 6.9z" fill="#000"/><circle cx="11.4" cy="3.1" r="1.25" fill="#000"/><path d="M10.4 9.1h11.3a1.25 1.25 0 0 1 0 2.5H10.4z" fill="#000"/></svg>
```

### boy.4 — Pacifier — tint `#8A84FF`
```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><path d="M15.8 20.53A7.9 7.9 0 1 0 8.2 20.53" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="2.2"/><path d="M3.9 11.9a1.75 1.75 0 0 0 0 3.5z" fill="#000"/><path d="M20.1 11.9a1.75 1.75 0 0 1 0 3.5z" fill="#000"/><ellipse cx="9.1" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><ellipse cx="14.9" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><ellipse cx="12" cy="16.85" rx="2.4" ry="1.1" fill="#000"/><circle cx="12" cy="20.7" r="1.5" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5"/></svg>
```

### girl.1 — Big bow — tint `#FF86B8`
```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="13.6" r="7.9" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="2.2"/><path d="M3.9 11.9a1.75 1.75 0 0 0 0 3.5z" fill="#000"/><path d="M20.1 11.9a1.75 1.75 0 0 1 0 3.5z" fill="#000"/><ellipse cx="9.1" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><ellipse cx="14.9" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><path d="M10.5 17.2q1.5 1.25 3 0" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8"/><path d="M15.5 5.5L10.27 4.82C9.1 4.76 8.79 5.74 9.13 6.67L9.86 8.7C10.2 9.63 11.07 10.18 11.93 9.38Z" fill="#000"/><path d="M15.5 5.5L19.07 1.62C19.93 0.82 20.8 1.37 21.14 2.3L21.87 4.33C22.21 5.26 21.9 6.24 20.73 6.18Z" fill="#000"/><circle cx="15.5" cy="5.5" r="1.8" fill="#000"/></svg>
```

### girl.2 — Pigtail puffs — tint `#FFB35C`
```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="13.6" r="7.9" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="2.2"/><path d="M3.9 11.9a1.75 1.75 0 0 0 0 3.5z" fill="#000"/><path d="M20.1 11.9a1.75 1.75 0 0 1 0 3.5z" fill="#000"/><ellipse cx="9.1" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><ellipse cx="14.9" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><path d="M10.5 17.2q1.5 1.25 3 0" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8"/><path d="M11.3 4.9C7.6 5.2 5 7.6 4.4 10.9c2.4-.4 5.3-2.2 6.9-4.4z" fill="#000"/><path d="M12.7 4.9c3.7.3 6.3 2.7 6.9 6-2.4-.4-5.3-2.2-6.9-4.4z" fill="#000"/><path d="M5.2 6.6L5.1 9.18A2.1 2.1 0 1 1 2.65 7.03Z" fill="#000"/><path d="M18.8 6.6L21.35 7.03A2.1 2.1 0 1 1 18.9 9.18Z" fill="#000"/></svg>
```

### girl.3 — Headband + flower — tint `#D89BFF`
```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="13.6" r="7.9" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="2.2"/><path d="M3.9 11.9a1.75 1.75 0 0 0 0 3.5z" fill="#000"/><path d="M20.1 11.9a1.75 1.75 0 0 1 0 3.5z" fill="#000"/><ellipse cx="9.1" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><ellipse cx="14.9" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><path d="M10.5 17.2q1.5 1.25 3 0" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8"/><path d="M4.6 10.4C7 7.4 17 7.4 19.4 10.4" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8"/><path d="M3.9 3.1a1.3 1.3 0 1 1 2.6 0a1.3 1.3 0 1 1 -2.6 0zM6.28 4.83a1.3 1.3 0 1 1 2.6 0a1.3 1.3 0 1 1 -2.6 0zM5.37 7.62a1.3 1.3 0 1 1 2.6 0a1.3 1.3 0 1 1 -2.6 0zM2.43 7.62a1.3 1.3 0 1 1 2.6 0a1.3 1.3 0 1 1 -2.6 0zM1.52 4.83a1.3 1.3 0 1 1 2.6 0a1.3 1.3 0 1 1 -2.6 0zM3.6 5.6a1.6 1.6 0 1 1 3.2 0a1.6 1.6 0 1 1 -3.2 0zM4.4 5.6a0.8 0.8 0 1 0 1.6 0a0.8 0.8 0 1 0 -1.6 0z" fill="#000" fill-rule="nonzero"/></svg>
```

### girl.4 — Top-knot bun — tint `#FF6B6B`
```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="13.6" r="7.9" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="2.2"/><path d="M3.9 11.9a1.75 1.75 0 0 0 0 3.5z" fill="#000"/><path d="M20.1 11.9a1.75 1.75 0 0 1 0 3.5z" fill="#000"/><ellipse cx="9.1" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><ellipse cx="14.9" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><path d="M10.5 17.2q1.5 1.25 3 0" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8"/><circle cx="12" cy="3" r="2.9" fill="#000"/><path d="M9.1 4.2Q12 5 14.9 4.2V5.9Q12 5.2 9.1 5.9z" fill="#000"/><path d="M11.3 5C8 5.2 5.6 7.6 5 10.3 7.5 10 10.4 8.4 11.3 6.3z" fill="#000"/><path d="M12.7 5c3.3.2 5.7 2.6 6.3 5.3-2.5-.3-5.4-1.9-6.3-4z" fill="#000"/></svg>
```

### neutral — Newborn wisps — tint `#9AA4C9`
```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="13.6" r="7.9" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="2.2"/><path d="M3.9 11.9a1.75 1.75 0 0 0 0 3.5z" fill="#000"/><path d="M20.1 11.9a1.75 1.75 0 0 1 0 3.5z" fill="#000"/><ellipse cx="9.1" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><ellipse cx="14.9" cy="13.9" rx="1.15" ry="1.4" fill="#000"/><path d="M10.5 17.2q1.5 1.25 3 0" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8"/><path d="M12 5.4V2.9M10.2 5.7 9.4 3.5M13.8 5.7l.8-2.2" fill="none" stroke="#000" stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8"/></svg>
```
