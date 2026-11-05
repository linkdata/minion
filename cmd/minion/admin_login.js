// JaWS reloads the rotated guest session, so stop its socket before sign-in navigation.
document.querySelector('form[action="/admin/login"]')?.addEventListener('submit', () => {
    window.removeEventListener('DOMContentLoaded', jawsConnectWhenReady);
    window.removeEventListener('load', jawsConnectWhenReady);
    if (jaws instanceof WebSocket) {
        jaws.removeEventListener('message', jawsMessage);
    }
    jawsUnloading();
});
