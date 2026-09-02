# Brand

| File | Use |
| --- | --- |
| `mark.svg` | The mark alone. Favicons, avatars, anywhere square. Readable down to about 24 px |
| `logo-light.svg` | Mark plus wordmark, for light backgrounds |
| `logo-dark.svg` | Mark plus wordmark, for dark backgrounds |
| `social-card.svg` | The link preview. Edit this one |
| `social-card.png` | The same card at 1280&#215;640, because GitHub's social preview uploader takes a raster |

The mark is a forebay: the basin upstream of a hydro turbine that holds enough water for the turbine
never to starve. Indigo is the basin, teal is what it holds, gold is the flow leaving it for the
turbine. Those three roles carry the same colours throughout the diagrams, so indigo always means
control, teal always means the fast tier, and gold always means capacity that is not Forebay's.

In markdown, pick the variant with `<picture>` so it follows the reader's theme:

```html
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/brand/logo-dark.svg">
  <img src="docs/brand/logo-light.svg" alt="Forebay" width="220">
</picture>
```

Regenerate the raster after editing the card, so the two cannot drift:

```sh
make social-card
```

GitHub has no per-repository avatar. The icon beside the repository name is the account's own,
set in profile settings, and `mark.svg` is the file to rasterise for it.

Please do not restretch the mark, recolour it, or set the wordmark in another typeface. Forking the
project is encouraged; forking the identity is confusing for everyone downstream.
