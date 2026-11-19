// Renders social/card.html to static/img/social-card.png at 1200x630, the size Open Graph and X expect.
//
//   node social/render.mjs                 # from www/
//   BROWSER=/path/to/chrome node social/render.mjs
//
// Uses a Chromium-family browser already on the machine in headless mode rather than a puppeteer
// dependency: the card changes rarely, and a 150 MB browser download in package-lock.json for every CI
// build of the docs is a poor trade for it. The PNG is committed, so nothing renders it at build time.
import {execFileSync} from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const source = path.join(here, 'card.html');
const target = path.join(here, '..', 'static', 'img', 'social-card.png');

const candidates = [
  process.env.BROWSER,
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
  '/usr/bin/microsoft-edge',
].filter(Boolean);

const browser = candidates.find((p) => fs.existsSync(p));
if (!browser) {
  console.error('no Chromium-family browser found; set BROWSER to one');
  process.exit(1);
}

// A throwaway profile under node_modules/.cache, so a browser the user already has open is neither reused nor disturbed.
const cache = path.join(here, '..', 'node_modules', '.cache');
fs.mkdirSync(cache, {recursive: true});
const profile = fs.mkdtempSync(path.join(cache, 'nfp-social-'));
try {
  execFileSync(browser, [
    '--headless=new',
    '--disable-gpu',
    '--hide-scrollbars',
    '--force-device-scale-factor=1',
    `--user-data-dir=${profile}`,
    '--window-size=1200,630',
    // Time for the fonts and the logo to load before the shot is taken.
    '--virtual-time-budget=5000',
    // card.html loads its fonts from node_modules beside it, which a file:// page may only do with this.
    '--allow-file-access-from-files',
    `--screenshot=${target}`,
    pathToFileURL(source).href,
  ], {stdio: 'ignore'});
} finally {
  fs.rmSync(profile, {recursive: true, force: true});
}

console.log(`wrote ${path.relative(process.cwd(), target)}`);
