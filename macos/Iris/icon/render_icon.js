// Renders icon.svg / icon-small.svg into AppIcon.iconset PNGs.
// Usage: node render_icon.js <dir with icon.svg, icon-small.svg> <out iconset dir>
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');
const [, , src, out] = process.argv;
// name -> pixel size; <= 64px uses the simplified small artwork.
const sizes = {
  'icon_16x16.png': 16, 'icon_16x16@2x.png': 32, 'icon_32x32.png': 32, 'icon_32x32@2x.png': 64,
  'icon_128x128.png': 128, 'icon_128x128@2x.png': 256, 'icon_256x256.png': 256,
  'icon_256x256@2x.png': 512, 'icon_512x512.png': 512, 'icon_512x512@2x.png': 1024,
};
(async () => {
  fs.mkdirSync(out, { recursive: true });
  const browser = await chromium.launch();
  for (const [name, px] of Object.entries(sizes)) {
    const svg = fs.readFileSync(path.join(src, px <= 64 ? 'icon-small.svg' : 'icon.svg'), 'utf8');
    const page = await browser.newPage({ viewport: { width: px, height: px }, deviceScaleFactor: 1 });
    await page.setContent(`<html><body style="margin:0;background:transparent">${svg.replace('<svg ', `<svg style="width:${px}px;height:${px}px;display:block" `)}</body></html>`);
    await page.locator('svg').screenshot({ path: path.join(out, name), omitBackground: true });
    await page.close();
  }
  await browser.close();
})();
