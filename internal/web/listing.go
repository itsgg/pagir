package web

import "html/template"

var listing = template.Must(template.New("listing").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>{{.Title}}</title>
<style>
:root { --bg: #fdfdfc; --fg: #1d1d1b; --muted: #6b6b66; --line: #e4e4e0; --link: #1a5fb4; }
@media (prefers-color-scheme: dark) {
  :root { --bg: #161615; --fg: #e8e8e4; --muted: #9a9a94; --line: #2c2c2a; --link: #78aeed; }
}
* { box-sizing: border-box; }
body { margin: 0 auto; max-width: 960px; padding: 24px 16px; background: var(--bg); color: var(--fg);
  font: 15px/1.5 system-ui, sans-serif; }
h1 { font-size: 18px; font-weight: 600; margin: 0 0 16px; overflow-wrap: anywhere; }
a { color: var(--link); text-decoration: none; }
a:hover { text-decoration: underline; }
.bar { display: flex; flex-wrap: wrap; gap: 12px; align-items: center; margin-bottom: 16px; }
table { width: 100%; border-collapse: collapse; }
td { padding: 6px 8px; border-top: 1px solid var(--line); vertical-align: top; }
td.name { overflow-wrap: anywhere; }
td.size, td.time { color: var(--muted); white-space: nowrap; text-align: right; }
@media (max-width: 560px) { td.time { display: none; } }
form { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
.empty { color: var(--muted); }
</style>
</head>
<body>
<h1>{{.Title}}</h1>
<div class="bar">
  <a href="?zip">Download all as zip</a>
  {{if .Upload}}
  <form method="post" enctype="multipart/form-data">
    <input type="file" name="file" multiple required>
    <button type="submit">Upload</button>
  </form>
  {{end}}
</div>
<table>
{{if .Up}}<tr><td class="name"><a href="../">../</a></td><td class="size"></td><td class="time"></td></tr>{{end}}
{{range .Entries}}<tr><td class="name"><a href="{{.Href}}">{{.Name}}</a></td><td class="size">{{.Size}}</td><td class="time">{{.Modified}}</td></tr>
{{else}}<tr><td class="empty" colspan="3">Empty folder</td></tr>
{{end}}
</table>
</body>
</html>
`))
