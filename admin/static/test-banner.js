const { JSDOM } = require('jsdom');
const fs = require('fs');
const path = require('path');

const i18nSrc = fs.readFileSync(path.join(__dirname, 'i18n.js'), 'utf8');
const appSrc = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');

const html = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Proxies - Phaethon Manager</title>
</head>
<body>
    <div class="app-layout">
        <nav class="sidebar">
            <ul class="nav-menu">
                <li><a href="./">Dashboard</a></li>
                <li><a href="./proxies">Proxies</a></li>
            </ul>
        </nav>
        <main class="main-content">
            <header class="top-bar">
                <span id="sse-status" class="badge badge-error hidden"></span>
                <span id="uptime" class="badge"></span>
                <span id="conn-count" class="badge badge-info"></span>
            </header>
            <div class="page-content"></div>
        </main>
    </div>
</body>
</html>`;

const dom = new JSDOM(html, {
    url: 'http://127.0.0.1:18080/layer/proxies',
    runScripts: 'dangerously',
    pretendToBeVisual: true,
});

const win = dom.window;
const doc = win.document;

win.console.error = (...args) => console.error('[PAGE ERR]', ...args);
win.console.log = (...args) => console.log('[PAGE LOG]', ...args);

console.log('EventSource present:', typeof win.EventSource);
console.log('localStorage present:', typeof win.localStorage);
console.log('i18n present after i18n load:', typeof win.i18n);

const bootTimes = ['1787571675520507875', '1787571675520507876'];

// Mock EventSource so SSE path is exercised without waiting for health timeout.
class MockEventSource {
    constructor(url) {
        this.url = url;
        this.readyState = 0;
        this._handlers = {};
        this._opened = false;
        // First heartbeat with the initial bootTime.
        setTimeout(() => this._boot(), 50);
    }
    _boot() {
        this.readyState = 1;
        this._opened = true;
        if (this.onopen) this.onopen();
        this._emit('heartbeat', { data: JSON.stringify({ _bootTime: bootTimes[0], stats: 1 }) });
        // Second heartbeat with a changed bootTime after a short delay.
        setTimeout(() => {
            this._emit('heartbeat', { data: JSON.stringify({ _bootTime: bootTimes[1], stats: 2 }) });
        }, 300);
    }
    addEventListener(type, handler) {
        if (!this._handlers[type]) this._handlers[type] = [];
        this._handlers[type].push(handler);
    }
    _emit(type, event) {
        (this._handlers[type] || []).forEach(h => h(event));
    }
    close() { this.readyState = 2; }
}
win.EventSource = MockEventSource;

win.fetch = async (url, opts) => {
    if (String(url).includes('./api/versions') || String(url).includes('/api/versions')) {
        return {
            ok: true,
            status: 200,
            headers: new win.Headers({ 'content-type': 'application/json' }),
            json: async () => ({ _bootTime: bootTimes[1], stats: 99 })
        };
    }
    return {
        ok: false,
        status: 404,
        headers: new win.Headers(),
        text: async () => 'not found'
    };
};

function runScript(src, name) {
    const script = doc.createElement('script');
    script.textContent = `
try {\n${src}\n} catch (e) { console.error('[SCRIPT ERR ${name}]', e.message, e.stack); }`;
    doc.head.appendChild(script);
}

runScript(i18nSrc, 'i18n');
console.log('i18n keys:', Object.keys(win).filter(k => k.includes('i18n') || k.includes('I18N')));
console.log('has i18n:', win.hasOwnProperty('i18n'));

runScript(appSrc, 'app');

console.log('VersionNotificationService present:', typeof win.VersionNotificationService);
console.log('registerDefaultVersionHandlers present:', typeof win.registerDefaultVersionHandlers);

// Dispatch DOMContentLoaded so app.js init runs.
const event = new win.Event('DOMContentLoaded', { bubbles: true });
doc.dispatchEvent(event);

function assert(cond, msg) {
    if (!cond) throw new Error('ASSERT FAIL: ' + msg);
}

async function runChecks() {
    // After first heartbeat (50ms) but before second (350ms).
    await new Promise(r => setTimeout(r, 150));

    const banner1 = doc.getElementById('server-restart-banner');
    assert(!banner1, 'banner should NOT exist after first bootTime (initial load)');

    // Wait for the second SSE heartbeat with the new bootTime.
    await new Promise(r => setTimeout(r, 300));

    const banner2 = doc.getElementById('server-restart-banner');
    assert(!!banner2, 'banner SHOULD exist after bootTime changes');

    const text = banner2.textContent;
    assert(text.includes('服务器已重启') || text.includes('Server restarted'), 'banner text should mention restart');

    const refreshBtn = banner2.querySelector('.server-restart-btn');
    assert(!!refreshBtn, 'refresh button should exist');

    const closeBtn = banner2.querySelector('.server-restart-close');
    assert(!!closeBtn, 'close button should exist');

    // Simulate clicking close button.
    closeBtn.click();
    assert(!doc.getElementById('server-restart-banner'), 'banner should be removed after close click');

    console.log('PASS: server-restart banner behaves as expected');
    process.exit(0);
}

runChecks().catch(err => {
    console.error('FAIL:', err.message);
    process.exit(1);
});
