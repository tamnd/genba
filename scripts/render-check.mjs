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
      const { shapeOf } = await import('genba/content.js');
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
      const { render } = await import('genba/html.js');
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
      const { render } = await import('genba/html.js');
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
      const { body, detailOf } = await import('genba/content.js');
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
      const { body } = await import('genba/content.js');
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
      const { body } = await import('genba/content.js');
      const out = body({ id: 'repo:broken.ipynb', media_type: 'application/x-ipynb+json', body: '{oops' });
      return Boolean(out.querySelector('.plain')) && out.textContent.includes('{oops');
    `),
  );

  // The sixth shape. A row and a grid cell ask for two different pictures and
  // neither of them is the file: a photograph off a phone is four megabytes and
  // the box it goes in is fifty six pixels across, so a list of twenty of them
  // downloaded as they are is eighty megabytes to draw a page.
  //
  // The only check here that watches something happen rather than reading what
  // a renderer returned, and the two ways that goes wrong are both handled in
  // the expression instead of by the retry above it. The request is issued from
  // an intersection callback, which is a frame away on an idle machine and
  // further than that on a loaded one, so it is waited for rather than slept
  // through. And the module holds one fetch per id and size for as long as the
  // page lives, so an id reused on a retry is answered out of that cache and
  // the retry sees no request at all, which turns a slow first attempt into a
  // failure nothing can recover from. Each attempt asks about a picture nobody
  // has asked about yet.
  await check(
    session,
    "a picture in a row and a picture in a grid cell are thumbnails of two sizes, not the file",
    expr(`
      const { tile, cover, TILE, CELL } = await import('genba/content.js');
      const n = (window.__thumbnails = (window.__thumbnails || 0) + 1);
      const hit = {
        id: 'repo:shot-' + n + '.png',
        media_type: 'image/png',
        kind: 'image',
        title: 'shot.png',
        modified_at: '7',
      };
      const asked = [];
      const sent = window.fetch;
      window.fetch = function (input) { asked.push(String((input && input.url) || input)); return sent.apply(this, arguments); };
      const boxes = [tile(hit), cover(hit)];
      const thumbs = () => asked.filter((u) => u.includes('/thumbnail?size='));
      try {
        document.body.append(boxes[0], boxes[1]);
        const until = Date.now() + 5000;
        while (thumbs().length < 2 && Date.now() < until) {
          await new Promise((done) => setTimeout(done, 50));
        }
      } finally {
        window.fetch = sent;
        for (const box of boxes) box.remove();
      }
      return thumbs().some((u) => u.includes('size=' + TILE)) &&
        thumbs().some((u) => u.includes('size=' + CELL)) &&
        thumbs().every((u) => u.includes('v=7')) &&
        !asked.some((u) => /\\/documents\\/[^/]+\\/content/.test(u)) &&
        TILE < CELL;
    `),
  );

  await check(
    session,
    "a document that is not a picture gets the icon for its kind rather than a hole in the grid",
    expr(`
      const { tile, cover } = await import('genba/content.js');
      const hit = { id: 'repo:notes.md', media_type: 'text/markdown', kind: 'document', source: 'repo' };
      return Boolean(tile(hit).querySelector('svg')) && !tile(hit).querySelector('.image') &&
        cover(hit).classList.contains('cover--icon');
    `),
  );

  await check(
    session,
    "a recording is its transcript, with a player that has not loaded anything yet",
    expr(`
      const { body } = await import('genba/content.js');
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
      const { body } = await import('genba/content.js');
      const out = body({ id: 'repo:clip.mp4', media_type: 'video/mp4', body: '' });
      return out.querySelector('.preview__empty').textContent.includes('Nothing was transcribed') &&
        out.querySelector('.media__bar button').textContent === 'Play the video';
    `),
  );

  await check(
    session,
    "code is highlighted with its language named, and everything else is shown plainly",
    expr(`
      const { body } = await import('genba/content.js');
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
      const { body } = await import('genba/content.js');
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
    "a file shows a number on every line and each one is a link to that line",
    expr(`
      const { body } = await import('genba/content.js');
      const out = body({ id: 'repo:main.go', media_type: 'text/x-go', body: 'package main\\n\\nfunc main() {}\\n' });
      const numbers = [...out.querySelectorAll('.line__no')];
      return numbers.length === 4 &&
        numbers.map((a) => a.textContent).join(',') === '1,2,3,4' &&
        numbers[3].getAttribute('href') === '#L4' &&
        numbers[0].getAttribute('aria-label') === 'Line 1';
    `),
  );

  // The reason the whole file goes through the lexer in one pass. Highlighting
  // a file a line at a time is faster to write and gets this wrong from the
  // second line of the comment onwards, along with every raw string and every
  // docstring.
  await check(
    session,
    "a comment that runs over several lines is one comment on all of them",
    expr(`
      const { rows } = await import('genba/highlight.js');
      const lines = rows('/*\\n one\\n two\\n*/\\nfunc main() {}', 'go');
      return lines.length === 5 &&
        lines.slice(0, 4).every((line) => line.querySelector('.tok--comment')) &&
        lines[4].querySelector('.tok--keyword').textContent === 'func';
    `),
  );

  await check(
    session,
    "an address that names a line opens the file at it and marks which one",
    expr(`
      const { body } = await import('genba/content.js');
      const { reveal } = await import('genba/marks.js');
      const source = Array.from({ length: 60 }, (_, i) => 'line ' + i).join('\\n');
      const out = body({ id: 'repo:main.go', media_type: 'text/x-go', body: source });
      const at = reveal(out, '#L42');
      return Boolean(at) && at.dataset.line === '42' &&
        at.classList.contains('line--current') &&
        out.querySelectorAll('.line--current').length === 1;
    `),
  );

  // Following a link to a line in the file already on screen changes nothing but
  // the address, so the move to that line is the only thing that can answer it.
  await check(
    session,
    "a line address that arrives on an open file moves to that line and no further",
    expr(`
      const { body } = await import('genba/content.js');
      const { reveal, toLine } = await import('genba/marks.js');
      const source = Array.from({ length: 60 }, (_, i) => 'line ' + i).join('\\n');
      const out = body({ id: 'repo:main.go', media_type: 'text/x-go', body: source });
      reveal(out, '#L42');
      const moved = toLine(out, '#L7');
      const stayed = toLine(out, '#main');
      return moved.dataset.line === '7' && stayed === null &&
        out.querySelectorAll('.line--current').length === 1 &&
        out.querySelector('.line--current').dataset.line === '7';
    `),
  );

  await check(
    session,
    "a document opened from a search opens at the first of the words somebody typed",
    expr(`
      const { body } = await import('genba/content.js');
      const { reveal, terms } = await import('genba/marks.js');
      const out = body(
        {
          id: 'repo:notes.md',
          media_type: 'text/markdown',
          body: '# Notes\\n\\nThe first paragraph.\\n\\nThe cache is warmed at start up.\\n',
        },
        { query: 'source:repo cache -cold' },
      );
      const marks = [...out.querySelectorAll('mark.hit')];
      return terms('source:repo cache -cold').join() === 'cache' &&
        marks.length === 1 && marks[0].textContent === 'cache' &&
        reveal(out, '') === marks[0];
    `),
  );

  // A line number is a coordinate rather than a word in the file, so a query
  // that happens to be a number must not paint the gutter with itself.
  await check(
    session,
    "a search for a number marks the code and not the line numbers",
    expr(`
      const { body } = await import('genba/content.js');
      const lines = Array.from({ length: 60 }, () => 'const x = 1');
      lines[2] = 'const limit = 42';
      const out = body(
        { id: 'repo:main.go', media_type: 'text/x-go', body: lines.join('\\n') },
        { query: '42' },
      );
      const marks = [...out.querySelectorAll('mark.hit')];
      return out.querySelectorAll('.line').length === 60 &&
        marks.length === 1 && marks[0].closest('.line').dataset.line === '3';
    `),
  );

  // The answer region, whose first duty is to not be there. Every search that
  // has nothing worth quoting has to leave the page exactly as it was, so the
  // empty case is asserted before the drawn one.
  await check(
    session,
    "the answer region is absent until there is something quoted, and then it quotes",
    expr(`
      const { Answer } = await import('genba/answer.js');
      const hits = [{ id: 'x', title: 'Runbook', source: 'repo' }];
      const a = new Answer({ onCite: () => {} });
      a.render({ hits, answer: null });
      const gone = a.el.hidden && a.el.childNodes.length === 0;
      a.render({
        hits,
        answer: { quotes: [{ id: 'x', text: 'The cache is warmed at start up.', passages: [
          { text: 'The ' }, { text: 'cache', match: true }, { text: ' is warmed at start up.' },
        ] }] },
      });
      const cite = a.el.querySelector('.quote__cite');
      return gone && !a.el.hidden &&
        a.el.querySelectorAll('.answer__quote').length === 1 &&
        a.el.querySelector('.quote__text').textContent === 'The cache is warmed at start up.' &&
        a.el.querySelector('.quote__text mark.hit').textContent === 'cache' &&
        cite.getAttribute('aria-label') === 'Open Runbook at this passage' &&
        cite.getAttribute('href').includes('at=') && cite.getAttribute('href').includes('open=x');
    `),
  );

  // A quote whose document is not on the page below it has nothing to cite, and
  // a citation that leads nowhere is the failure the whole region is built to
  // avoid.
  await check(
    session,
    "a quote is dropped when the document it cites is not on the page",
    expr(`
      const { Answer } = await import('genba/answer.js');
      const a = new Answer({ onCite: () => {} });
      a.render({
        hits: [{ id: 'y', title: 'Notes', source: 'repo' }],
        answer: { quotes: [{ id: 'x', text: 'The cache is warmed at start up.' }] },
      });
      return a.el.hidden && a.el.childNodes.length === 0;
    `),
  );

  // The quote was cut out of the source, where the whitespace is whatever the
  // author typed, and it is looked for in the rendered document, where the same
  // sentence is split across three nodes by a bold word. Neither side's spacing
  // is the other's, so neither side's spacing is compared.
  await check(
    session,
    "a cited passage is found through inline markup and through changed whitespace",
    expr(`
      const { passage } = await import('genba/marks.js');
      const box = document.createElement('div');
      box.innerHTML = '<p>The <b>cache</b>\\n  is warmed\\n  at start up.</p><p>Nothing else.</p>';
      const at = passage(box, 'The cache  is warmed at start up.');
      const squeeze = (s) => s.replace(/\\s+/g, '');
      const marks = [...box.querySelectorAll('mark.hit--passage')];
      const other = document.createElement('div');
      other.innerHTML = '<p>A paragraph about something entirely different.</p>';
      return Boolean(at) && marks.length === 3 &&
        squeeze(marks.map((m) => m.textContent).join('')) === squeeze('The cache is warmed at start up.') &&
        squeeze(box.textContent) === squeeze('The cache is warmed at start up. Nothing else.') &&
        passage(other, 'The cache is warmed at start up.') === null;
    `),
  );

  await check(
    session,
    "a document opened from a citation opens at the sentence rather than at the first word",
    expr(`
      const { body } = await import('genba/content.js');
      const out = body(
        {
          id: 'repo:notes.md',
          media_type: 'text/markdown',
          body: '# Notes\\n\\nThe first paragraph.\\n\\nThe cache is warmed at start up.\\n',
        },
        { query: 'cache', at: 'The cache is warmed at start up.' },
      );
      const cited = out.querySelector('mark.hit--passage');
      return Boolean(cited) &&
        cited.textContent === 'The cache is warmed at start up.' &&
        out.querySelectorAll('mark.hit').length === 1;
    `),
  );

  await check(
    session,
    "a body the extractor could not read to the end says which half is missing",
    expr(`
      const { body } = await import('genba/content.js');
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
