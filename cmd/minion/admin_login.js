// JaWS reloads retired sessions, so stop its socket before form navigation starts.
document.querySelector('form[action="/admin/login"], form[action="/admin/logout"]')?.addEventListener('submit', () => {
    window.removeEventListener('DOMContentLoaded', jawsConnectWhenReady);
    window.removeEventListener('load', jawsConnectWhenReady);
    if (jaws instanceof WebSocket) {
        jaws.removeEventListener('message', jawsMessage);
    }
    jawsUnloading();
});
