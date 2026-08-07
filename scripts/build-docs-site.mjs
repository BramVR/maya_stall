import fs from "node:fs";
import path from "node:path";

const root = path.resolve(new URL("..", import.meta.url).pathname);
const docsDir = path.join(root, "docs");
const outDir = path.join(root, "dist", "docs-site");
const siteName = "Maya Stall Docs";
const repoEditBase = "https://github.com/BramVR/maya_stall/edit/main/docs";
const repoSourceBase = "https://github.com/BramVR/maya_stall/blob/main";

const sections = [
  ["Start", ["README.md", "getting-started.md", "cli.md", "concepts.md", "source-map.md"]],
  ["Commands", rels("commands")],
  ["Setup", rels("setup")],
  ["Agents", rels("agents")],
  ["Product", [...rels("prd"), ...rels("adr")]],
];

const markdownFiles = allMarkdown(docsDir);
const pages = markdownFiles.map((file) => {
  const relPath = path.relative(docsDir, file);
  const source = fs.readFileSync(file, "utf8");
  return {
    relPath,
    title: firstHeading(source) ?? titleize(relPath),
    source,
    url: pageUrl(relPath),
  };
});

fs.rmSync(outDir, { recursive: true, force: true });
fs.mkdirSync(outDir, { recursive: true });

const pageByRel = new Map(pages.map((page) => [page.relPath, page]));
const directoryIndexes = buildDirectoryIndexes();

for (const page of pages) {
  const output = path.join(outDir, page.url);
  fs.mkdirSync(path.dirname(output), { recursive: true });
  fs.writeFileSync(output, layout(page, markdownToHtml(page.source, page.relPath)), "utf8");
}

for (const index of directoryIndexes) {
  const output = path.join(outDir, index.url);
  fs.mkdirSync(path.dirname(output), { recursive: true });
  fs.writeFileSync(output, layout(index, index.html), "utf8");
}

fs.writeFileSync(path.join(outDir, ".nojekyll"), "", "utf8");
fs.writeFileSync(path.join(outDir, "llms.txt"), llmsTxt(), "utf8");
fs.writeFileSync(path.join(outDir, "maya-stall.svg"), logoSvg(), "utf8");

console.log(`built docs site: ${path.relative(root, outDir)}`);

function rels(dir) {
  const fullDir = path.join(docsDir, dir);
  if (!fs.existsSync(fullDir)) {
    return [];
  }
  return allMarkdown(fullDir).map((file) => path.relative(docsDir, file));
}

function allMarkdown(dir) {
  const files = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const fullPath = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...allMarkdown(fullPath));
    } else if (entry.name.endsWith(".md")) {
      files.push(fullPath);
    }
  }
  return files.sort((a, b) => path.relative(docsDir, a).localeCompare(path.relative(docsDir, b)));
}

function pageUrl(relPath) {
  if (relPath === "README.md") {
    return "index.html";
  }
  if (path.basename(relPath) === "README.md") {
    return `${path.dirname(relPath)}/index.html`;
  }
  return relPath.replace(/\.md$/, ".html");
}

function buildDirectoryIndexes() {
  const dirs = new Map();
  for (const page of pages) {
    const dir = path.dirname(page.relPath);
    if (dir !== ".") {
      if (!dirs.has(dir)) {
        dirs.set(dir, []);
      }
      dirs.get(dir).push(page);
    }
  }

  const indexes = [];
  for (const [dir, dirPages] of [...dirs.entries()].sort()) {
    if (pageByRel.has(`${dir}/README.md`)) {
      continue;
    }
    const title = titleize(dir);
    const list = dirPages
      .map((page) => `<li><a href="${relativeHref(`${dir}/index.html`, page.url)}">${escapeHtml(page.title)}</a></li>`)
      .join("\n");
    indexes.push({
      relPath: `${dir}/`,
      title,
      url: `${dir}/index.html`,
      source: `# ${title}`,
      html: `<h1>${escapeHtml(title)}</h1>\n<ul>\n${list}\n</ul>`,
      generated: true,
    });
  }
  return indexes;
}

function firstHeading(source) {
  const match = source.match(/^#\s+(.+)$/m);
  return match ? stripMarkdown(match[1]).trim() : null;
}

function titleize(relPath) {
  const base = relPath
    .replace(/\/README\.md$/, "")
    .replace(/\.md$/, "")
    .split("/")
    .at(-1)
    .replace(/^\d+-/, "")
    .replace(/-/g, " ");
  return base.replace(/\b\w/g, (char) => char.toUpperCase());
}

function markdownToHtml(source, relPath) {
  const lines = source.replace(/\r\n/g, "\n").split("\n");
  const html = [];
  let paragraph = [];
  let list = null;
  let fence = null;
  let table = [];

  const flushParagraph = () => {
    if (paragraph.length === 0) {
      return;
    }
    html.push(`<p>${inline(paragraph.join(" "), relPath)}</p>`);
    paragraph = [];
  };
  const flushList = () => {
    if (!list) {
      return;
    }
    html.push(`<${list.type}>\n${list.items.join("\n")}\n</${list.type}>`);
    list = null;
  };
  const flushTable = () => {
    if (table.length === 0) {
      return;
    }
    const rows = table.map((line) => line.trim().replace(/^\||\|$/g, "").split("|").map((cell) => cell.trim()));
    const header = rows[0] ?? [];
    const body = rows.slice(2);
    html.push(`<table><thead><tr>${header.map((cell) => `<th>${inline(cell, relPath)}</th>`).join("")}</tr></thead><tbody>${body.map((row) => `<tr>${row.map((cell) => `<td>${inline(cell, relPath)}</td>`).join("")}</tr>`).join("")}</tbody></table>`);
    table = [];
  };

  for (const line of lines) {
    const fenceMatch = line.match(/^```(.*)$/);
    if (fenceMatch) {
      flushParagraph();
      flushList();
      flushTable();
      if (fence) {
        html.push(`<pre><code${fence.lang ? ` class="language-${escapeAttribute(fence.lang)}"` : ""}>${escapeHtml(fence.lines.join("\n"))}</code></pre>`);
        fence = null;
      } else {
        fence = { lang: fenceMatch[1].trim(), lines: [] };
      }
      continue;
    }
    if (fence) {
      fence.lines.push(line);
      continue;
    }

    if (/^\s*$/.test(line)) {
      flushParagraph();
      flushList();
      flushTable();
      continue;
    }

    if (/^\|.+\|$/.test(line)) {
      flushParagraph();
      flushList();
      table.push(line);
      continue;
    }
    flushTable();

    const listContinuation = line.match(/^\s{2,}(.+)$/);
    if (list && listContinuation) {
      const index = list.items.length - 1;
      list.items[index] = list.items[index].replace(/<\/li>$/, ` ${inline(listContinuation[1], relPath)}</li>`);
      continue;
    }

    const heading = line.match(/^(#{1,6})\s+(.+)$/);
    if (heading) {
      flushParagraph();
      flushList();
      const level = heading[1].length;
      const text = stripMarkdown(heading[2]).trim();
      html.push(`<h${level} id="${escapeAttribute(slugify(text))}">${inline(heading[2], relPath)}</h${level}>`);
      continue;
    }

    const unordered = line.match(/^\s*-\s+(.+)$/);
    const ordered = line.match(/^\s*\d+\.\s+(.+)$/);
    if (unordered || ordered) {
      flushParagraph();
      const type = unordered ? "ul" : "ol";
      if (!list || list.type !== type) {
        flushList();
        list = { type, items: [] };
      }
      list.items.push(`<li>${inline((unordered ?? ordered)[1], relPath)}</li>`);
      continue;
    }

    paragraph.push(line.trim());
  }

  flushParagraph();
  flushList();
  flushTable();
  return html.join("\n");
}

function inline(text, relPath) {
  const code = [];
  const protectedText = text.replace(/`([^`]+)`/g, (_, value) => {
    const token = `@@CODE${code.length}@@`;
    code.push(`<code>${escapeHtml(value)}</code>`);
    return token;
  });
  let rendered = escapeHtml(protectedText);

  rendered = rendered
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_, label, href) => {
      return `<a href="${escapeAttribute(rewriteHref(href, relPath))}">${label}</a>`;
    });

  for (const [index, value] of code.entries()) {
    rendered = rendered.replace(`@@CODE${index}@@`, value);
  }
  return rendered;
}

function rewriteHref(href, relPath) {
  const target = stripAngleBrackets(href.trim());
  if (/^(https?:|mailto:)/.test(target) || target.startsWith("#")) {
    return target;
  }
  const hashIndex = target.indexOf("#");
  const pathname = hashIndex === -1 ? target : target.slice(0, hashIndex);
  const hash = hashIndex === -1 ? "" : target.slice(hashIndex);
  if (pathname === "") {
    return hash;
  }

  const sourceDir = path.dirname(relPath);
  const absoluteTarget = path.resolve(docsDir, sourceDir, pathname);
  if (!absoluteTarget.startsWith(docsDir)) {
    return `${repoSourceBase}/${path.relative(root, absoluteTarget).replaceAll(path.sep, "/")}${hash}`;
  }
  let targetUrl;
  if (fs.existsSync(absoluteTarget) && fs.statSync(absoluteTarget).isDirectory()) {
    targetUrl = `${path.relative(docsDir, absoluteTarget)}/index.html`;
  } else if (pathname.endsWith(".md")) {
    targetUrl = pageUrl(path.relative(docsDir, absoluteTarget));
  } else {
    return target;
  }
  return `${relativeHref(pageUrl(relPath), targetUrl)}${hash}`;
}

function relativeHref(fromUrl, toUrl) {
  const fromDir = path.dirname(fromUrl);
  let relative = path.relative(fromDir === "." ? "" : fromDir, toUrl).replaceAll(path.sep, "/");
  if (!relative.startsWith(".")) {
    relative = `./${relative}`;
  }
  return relative;
}

function layout(page, body) {
  const nav = sections.map(([name, relPaths]) => {
    const items = relPaths
      .filter((relPath) => pageByRel.has(relPath))
      .map((relPath) => {
        const navPage = pageByRel.get(relPath);
        const active = navPage.relPath === page.relPath ? " aria-current=\"page\"" : "";
        return `<a${active} href="${relativeHref(page.url, navPage.url)}">${escapeHtml(navPage.title)}</a>`;
      })
      .join("");
    return `<section><h2>${escapeHtml(name)}</h2>${items}</section>`;
  }).join("");
  const footer = page.generated
    ? ""
    : `<footer><a href="${escapeAttribute(`${repoEditBase}/${page.relPath}`)}">Edit this page</a></footer>`;
  const logoHref = relativeHref(page.url, "index.html");
  const logoSrc = relativeHref(page.url, "maya-stall.svg");

  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>${escapeHtml(page.title)} - ${siteName}</title>
  <style>${css()}</style>
</head>
<body>
  <aside>
    <a class="brand" href="${logoHref}">
      <img alt="" src="${logoSrc}" width="32" height="32">
      <span>${siteName}</span>
    </a>
    <nav>${nav}</nav>
  </aside>
  <main>
    <article>${body}</article>
    ${footer}
  </main>
</body>
</html>
`;
}

function llmsTxt() {
  return `# ${siteName}

Maya Stall is a Go CLI for running real Autodesk Maya UI scenarios from repo-owned config.

## Docs

${pages.map((page) => `- [${page.title}](${page.url})`).join("\n")}
`;
}

function css() {
  return `
:root {
  color-scheme: light dark;
  --bg: #f7f5ef;
  --panel: #ffffff;
  --text: #202123;
  --muted: #5e6470;
  --line: #d8d1c2;
  --accent: #246b5b;
  --code: #f1eee7;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #141414;
    --panel: #1d1d1f;
    --text: #f2efe7;
    --muted: #b5afa4;
    --line: #393630;
    --accent: #6fd0b7;
    --code: #292724;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0;
  background: var(--bg);
  color: var(--text);
  font: 16px/1.6 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
a { color: var(--accent); }
aside {
  background: var(--panel);
  border-right: 1px solid var(--line);
  bottom: 0;
  overflow: auto;
  padding: 24px;
  position: fixed;
  top: 0;
  width: 300px;
}
.brand {
  align-items: center;
  color: var(--text);
  display: flex;
  font-weight: 700;
  gap: 12px;
  margin-bottom: 28px;
  text-decoration: none;
}
nav section { margin: 0 0 24px; }
nav h2 {
  color: var(--muted);
  font-size: 12px;
  letter-spacing: 0;
  margin: 0 0 8px;
  text-transform: uppercase;
}
nav a {
  border-radius: 6px;
  color: var(--text);
  display: block;
  padding: 5px 8px;
  text-decoration: none;
}
nav a[aria-current="page"], nav a:hover { background: var(--code); }
main {
  margin-left: 300px;
  padding: 48px 32px 80px;
}
article {
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 8px;
  margin: 0 auto;
  max-width: 920px;
  padding: 40px;
}
h1, h2, h3 { line-height: 1.2; }
h1 { font-size: 36px; margin-top: 0; }
h2 { border-top: 1px solid var(--line); margin-top: 40px; padding-top: 28px; }
pre {
  background: var(--code);
  border-radius: 8px;
  overflow: auto;
  padding: 16px;
}
code {
  background: var(--code);
  border-radius: 4px;
  padding: 0.1em 0.3em;
}
pre code {
  background: transparent;
  padding: 0;
}
table {
  border-collapse: collapse;
  display: block;
  overflow: auto;
  width: 100%;
}
td, th {
  border: 1px solid var(--line);
  padding: 8px 10px;
  text-align: left;
}
footer {
  margin: 24px auto 0;
  max-width: 920px;
}
@media (max-width: 860px) {
  aside {
    border-bottom: 1px solid var(--line);
    border-right: 0;
    position: static;
    width: auto;
  }
  main {
    margin-left: 0;
    padding: 24px 16px 56px;
  }
  article {
    padding: 24px;
  }
}
`;
}

function logoSvg() {
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" role="img" aria-label="Maya Stall">
  <rect width="64" height="64" rx="14" fill="#246b5b"/>
  <path d="M17 43V19h8l7 12 7-12h8v24h-7V30l-6 10h-4l-6-10v13z" fill="#f7f5ef"/>
</svg>
`;
}

function stripMarkdown(value) {
  return value
    .replace(/`([^`]+)`/g, "$1")
    .replace(/\[([^\]]+)\]\([^)]+\)/g, "$1")
    .replace(/\*\*([^*]+)\*\*/g, "$1");
}

function slugify(text) {
  return text
    .toLowerCase()
    .replace(/[^\p{Letter}\p{Number}\s-]/gu, "")
    .trim()
    .replace(/\s+/g, "-")
    .replace(/-+/g, "-");
}

function stripAngleBrackets(target) {
  if (target.startsWith("<") && target.endsWith(">")) {
    return target.slice(1, -1);
  }
  return target;
}

function escapeHtml(value) {
  return value
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function escapeAttribute(value) {
  return escapeHtml(value).replace(/'/g, "&#39;");
}
