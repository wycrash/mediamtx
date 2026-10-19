import { parseOptions, resolveLiveProto } from './api.js?v=13';
import { MediaMtxPlayer } from './player.js?v=61';

function ensureStyles() {
    const href = new URL('../css/player.css?v=27', import.meta.url).href;
    if (!document.querySelector(`link[data-mtx-dvr-css="${href}"]`)) {
        const link = document.createElement('link');
        link.rel = 'stylesheet';
        link.href = href;
        link.setAttribute('data-mtx-dvr-css', href);
        document.head.appendChild(link);
    }
}

export function load(options, container) {
    if (!container) throw new Error('MediaMtxDvrPlayer: container element is required');
    ensureStyles();
    const parsed = applyLiveProto(parseOptions(options), options);
    const player = new MediaMtxPlayer(parsed, container);
    const ready = player.mount();

    const api = {
        play: () => player.play(),
        stop: () => player.stop(),
        restartPlayer: () => player.restart(),
        changeLanguage: (lang) => player.changeLanguage(lang),
        goLive: () => player.goLive(),
        openArchive: (from, duration) => player.openArchive(from, duration),
        DvrUnmount: () => player.destroy(),
        player,
        ready,
    };

    return api;
}

const MediaMtxDvrPlayer = { load, default: { load } };
if (typeof window !== 'undefined') {
    window.MediaMtxDvrPlayer = MediaMtxDvrPlayer;
}

export default MediaMtxDvrPlayer;

function applyLiveProto(parsed, raw = {}) {
    const opts = parsed || {};
    const query = {};
    try {
        new URLSearchParams(String(raw.query || opts.query || '').replace(/^\?/, '')).forEach((value, key) => {
            query[key] = value;
        });
    } catch (_e) { /* ignore */ }

    opts.streamer_webrtc = String(opts.streamer_webrtc || raw.streamer_webrtc || query.streamer_webrtc || '').replace(/\/+$/, '');
    opts.streamer_http = String(opts.streamer_http || raw.streamer_http || query.streamer_http || '').replace(/\/+$/, '');
    opts.streamer_moq = String(opts.streamer_moq || raw.streamer_moq || query.streamer_moq || '').replace(/\/+$/, '');
    opts.proto = resolveLiveProto({
        proto: query.proto || raw.proto || opts.proto || 'webrtc',
        streamer_http: opts.streamer_http,
        streamer_webrtc: opts.streamer_webrtc,
        streamer_moq: opts.streamer_moq,
    });

    return opts;
}
