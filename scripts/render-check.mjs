// The rendering check.
//
// Every document in a corpus goes through one of six renderers, and five of the
// six cannot be reached from the repository this gate indexes: there is no
// notebook in it, no recording, no PDF and no page of generated HTML. So the
// renderers are called directly, with a fixture for each, in the same browser
// the rest of the gate uses.
//
// The browser is the test runner and there is nothing to install. These are ES
// modules served by the server that is already running, so an import in the
// page is the real module with the real DOM under it, which matters more here
// than anywhere else in the interface: the whole of html.js is a claim about
// what a real parser does with hostile markup, and a fake DOM would be a claim
// about the fake.

import { available, launch, reporter, visit } from "./chrome.mjs";

const BASE = process.argv[2] || "http://127.0.0.1:8123";

const why = available();
if (why) {
  console.log(`render-check: ${why}, so the rendering check cannot run`);
  process.exit(0);
}

// A page of HTML written by somebody who was testing whether the wiki escaped
// script tags, which is a document every real corpus turns out to contain. Half
// of it is content and half of it is the five ways markup gets a program onto
// somebody else's screen.
const HOSTILE = `<!DOCTYPE html>
<html><head><title>Report</title><style>body{color:red}</style></head>
<body onload="steal()">
  <script>window.taken = true</script>
  <h2 class="title">Quarterly report</h2>
  <section>
    <p onclick="steal()">The <b>numbers</b> for the quarter, with a <a href="https://example.com/detail">detail page</a>.</p>
    <p><a href="javascript:steal()">Click here</a> and <a href="data:text/html,<script>1</script>">here</a>.</p>
    <ul><li>One</li><li>Two</li></ul>
    <table><tr><th>Region</th><th>Total</th></tr><tr><td>North</td><td>12</td></tr></table>
    <pre class="language-go"><code>func main() {}</code></pre>
    <img src="data:image/svg+xml,<svg onload=steal()></svg>" alt="chart">
    <svg width="10" height="10"><script>window.taken = true</script></svg>
    <iframe src="https://example.com"></iframe>
  </section>
</body></html>`;

const NOTEBOOK = JSON.stringify({
  metadata: { language_info: { name: "python" } },
  cells: [
    { cell_type: "markdown", source: ["## Loading the data\n", "A short note.\n"] },
    {
      cell_type: "code",
      execution_count: 3,
      source: ["import pandas as pd\n", "df.head()\n"],
      outputs: [
        { output_type: "stream", name: "stdout", text: ["loaded 12 rows\n"] },
        {
          output_type: "display_data",
          data: {
            "image/png":
              "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
          },
        },
        { output_type: "display_data", data: { "application/vnd.jupyter.widget-view+json": {} } },
      ],
    },
  ],
});

const TABLE = ["| Name | Value |", "| --- | --- |"]
  .concat(Array.from({ length: 60 }, (_, i) => `| Row ${i} | ${i} |`))
  .join("\n");

const { state, check } = reporter("render-check");
const browser = await launch();
try {
  await run(browser.session);
} catch (err) {
  console.error(`render-check: ${err.message}`);
  state.failures++;
} finally {
  browser.stop();
}
process.exit(state.failures ? 1 : 0);

async function run(session) {
  // Any page of the interface will do. What is needed from it is the origin, so
  // that an import of a module resolves to the module the binary is serving.
  await visit(session, `${BASE}/`, "Boolean(document.querySelector('.app'))");

  await check(
    session,
    "every media type in the corpus lands on a renderer that suits it",
    expr(`
      const { shapeOf } = await import('/assets/content.js');
      const of = (media, id) => shapeOf({ media_type: media, id: id || 'repo:x' });
      return of('image/png') === 'image' &&
        of('audio/mpeg') === 'media' && of('video/mp4') === 'media' &&
        of('application/x-ipynb+json') === 'notebook' &&
        of('text/markdown') === 'prose' && of('text/html') === 'prose' &&
        of('application/pdf') === 'prose' &&
        of('text/x-go') === 'code' &&
        of('application/octet-stream') === 'text';
    `),
  );

  // The security assertion. Nothing in the fixture that is a program survives,
  // and everything in it that is a document does.
  await check(
    session,
    "nothing that can run survives the HTML renderer",
    expr(`
      const { render } = await import('/assets/html.js');
      const box = document.createElement('div');
      box.appendChild(render(${JSON.stringify(HOSTILE)}));
      const has = (sel) => Boolean(box.querySelector(sel));
      return !window.taken &&
        !has('script') && !has('svg') && !has('iframe') && !has('style') &&
        !has('[onclick]') && !has('[onload]') &&
        ![...box.querySelectorAll('a')].some((a) => a.getAttribute('href').startsWith('javascript:')) &&
        ![...box.querySelectorAll('img')].some((i) => i.getAttribute('src').startsWith('data:'));
    `),
  );

  await check(
    session,
    "everything in that page that was a document survives it",
    expr(`
      const { render } = await import('/assets/html.js');
      const box = document.createElement('div');
      box.appendChild(render(${JSON.stringify(HOSTILE)}));
      const link = box.querySelector('a[href="https://example.com/detail"]');
      return box.querySelector('h2').textContent === 'Quarterly report' &&
        box.querySelectorAll('p').length === 2 &&
        box.querySelector('strong').textContent === 'numbers' &&
        box.querySelectorAll('li').length === 2 &&
        box.querySelectorAll('td').length === 2 &&
        box.querySelector('.code__pre').textContent.includes('func main()') &&
        box.querySelector('.code__lang').textContent === 'go' &&
        Boolean(link) && link.rel === 'noreferrer noopener' &&
        box.textContent.includes('The numbers for the quarter');
    `),
  );

  await check(
    session,
    "a PDF is prose with its length beside it rather than a wall of text",
    expr(`
      const { body, detailOf } = await import('/assets/content.js');
      const out = body({
        id: 'repo:report.pdf',
        media_type: 'application/pdf',
        title: 'Report',
        body: '# Report\\n\\nThe first paragraph.\\n\\n- one\\n- two\\n',
        properties: { pages: '12' },
      });
      return out.querySelectorAll('.prose__p').length === 1 &&
        out.querySelectorAll('.prose__item').length === 2 &&
        detailOf({ properties: { pages: '12' } }) === '12 pages' &&
        detailOf({ properties: { pages: '1' } }) === '1 page' &&
        detailOf({ properties: {} }) === '';
    `),
  );

  await check(
    session,
    "a notebook is prose and code cells with the outputs it produced",
    expr(`
      const { body } = await import('/assets/content.js');
      const out = body({
        id: 'repo:analysis.ipynb',
        media_type: 'application/x-ipynb+json',
        title: 'analysis.ipynb',
        body: ${JSON.stringify(NOTEBOOK)},
      });
      const image = out.querySelector('.notebook__image');
      return out.querySelectorAll('.notebook__prose .prose__h').length === 1 &&
        out.querySelector('.notebook__count').textContent === '[3]' &&
        out.querySelector('.code__pre').textContent.includes('import pandas') &&
        out.querySelector('.notebook__out').textContent.includes('loaded 12 rows') &&
        Boolean(image) && image.getAttribute('src').startsWith('data:image/png;base64,') &&
        out.querySelector('.notebook__elided').textContent.includes('widget-view');
    `),
  );

  await check(
    session,
    "a notebook that is not valid JSON falls back to being shown as it is",
    expr(`
      const { body } = await import('/assets/content.js');
      const out = body({ id: 'repo:broken.ipynb', media_type: 'application/x-ipynb+json', body: '{oops' });
      return Boolean(out.querySelector('.plain')) && out.textContent.includes('{oops');
    `),
  );

  await check(
    session,
    "a recording is its transcript, with a player that has not loaded anything yet",
    expr(`
      const { body } = await import('/assets/content.js');
      const out = body({
        id: 'repo:standup.mp3',
        media_type: 'audio/mpeg',
        title: 'standup.mp3',
        body: 'We shipped the index.',
        properties: { size_bytes: '2400000' },
      });
      return out.textContent.includes('We shipped the index.') &&
        !out.querySelector('audio') && !out.querySelector('video') &&
        out.querySelector('.media__bar button').textContent === 'Play the recording' &&
        out.querySelector('.media__size').textContent === '2.4 MB';
    `),
  );

  await check(
    session,
    "a recording with nothing transcribed says so rather than looking empty",
    expr(`
      const { body } = await import('/assets/content.js');
      const out = body({ id: 'repo:clip.mp4', media_type: 'video/mp4', body: '' });
      return out.querySelector('.preview__empty').textContent.includes('Nothing was transcribed') &&
        out.querySelector('.media__bar button').textContent === 'Play the video';
    `),
  );

  await check(
    session,
    "code is highlighted with its language named, and everything else is shown plainly",
    expr(`
      const { body } = await import('/assets/content.js');
      const code = body({ id: 'repo:main.go', media_type: 'text/x-go', body: 'package main\\n' });
      const plain = body({ id: 'repo:data.bin', media_type: 'application/octet-stream', body: 'raw\\nbytes' });
      return code.querySelector('.code__lang').textContent === 'go' &&
        code.querySelector('.tok--keyword').textContent === 'package' &&
        plain.querySelector('.plain').textContent === 'raw\\nbytes';
    `),
  );

  await check(
    session,
    "a long table is folded to a screenful in a region the keyboard can scroll",
    expr(`
      const { body } = await import('/assets/content.js');
      const out = body({ id: 'repo:notes.md', media_type: 'text/markdown', body: ${JSON.stringify(TABLE)} });
      const region = out.querySelector('.prose__scroll');
      const shown = () => [...out.querySelectorAll('tbody tr')].filter((r) => !r.hidden).length;
      const before = shown();
      out.querySelector('.prose__more').click();
      return region.tabIndex === 0 && region.getAttribute('aria-label') === 'Table, 60 rows' &&
        before === 20 && shown() === 60 && !out.querySelector('.prose__more');
    `),
  );

  await check(
    session,
    "a body the extractor could not read to the end says which half is missing",
    expr(`
      const { body } = await import('/assets/content.js');
      const out = body({
        id: 'repo:big.pdf',
        media_type: 'application/pdf',
        body: 'The first page.',
        properties: { truncated: 'true' },
      });
      return out.querySelector('.preview__note').textContent.includes('too large to read in full');
    `),
  );
}

/** expr wraps a block of statements in something Runtime.evaluate can await. */
function expr(code) {
  return `(async () => {${code}})()`;
}
