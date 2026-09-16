# Admin Dashboard Split & Rules UI Tasks

## Background

Dashboard was overloaded with TUN controls, Mesh topology, connections, logs, etc.
Split TUN and Mesh into standalone pages, improve rules UX.

## Tasks

- [x] i18n: rename "地址重定向" to "重定向", add new translation keys (zh + en)
- [x] admin.go: add TUN/Mesh page routes and handlers
- [x] tun.html: create standalone TUN page with all controls
- [x] mesh.html: create standalone Mesh page with topology/routes/config
- [x] dashboard.html: replace full panels with lightweight summary cards
- [x] layout.html: add "Runtime" sidebar section with TUN/Mesh nav items
- [x] app.js: add PAGE_TITLES entries, summary update functions for SSE
- [x] rules.html: remove drag-and-drop, add insert above/below buttons
- [x] style.css: remove unused drag-and-drop CSS

## Deployed

- VM (10.21.20.65): verified
- QG (10.11.61.40): verified
- JF: N/A (h_tunnel only, no admin TUN/Mesh)
