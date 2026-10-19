import { joinUrl, pad, parseBool, parseControlsMin, parseExportLimit, parseFit, parseHideControls, parsePlayDuration, parseQuery, parseReturnUrl, parseUnix, withQuery } from './utils.js?v=7';

const DEFAULTS = {
    name: '',
    streamer_http: '',
    streamer_webrtc: '',
    streamer_moq: '',
    query: '',
    proto: 'webrtc',
    dvr: true,
    debug: false,
    locale: 'ru',
    autoPlay: true,
    muted: true,
    preview: true,
    hidecontrols: null,
    controls_min: 200,
    export: false,
    export_limit: 600,
    fit: 'auto',
    panzoom: false,
    fixedRange: false,
    from: null,
    to: null,
    play_duration: null,
    token: '',
    tokenName: 'token',
    user: '',
    pass: '',
    title: '',
    return_url: '',
    retryPause: 5000,
    centeredSeek: false,
    event: false,
};

export function parseProto(value, fallback = 'webrtc') {
    const str = String(value ?? '').toLowerCase().trim();
    if (['webrtc', 'whep', 'rtc'].includes(str)) return 'webrtc';
    if (['moq', 'quic', 'webtransport'].includes(str)) return 'moq';
    if (str === 'hls') return 'hls';
    return fallback;
}

export function resolveLiveProto(options = {}) {
    const want = parseProto(options.proto, 'webrtc');
    const has = {
        hls: Boolean(options.streamer_http),
        webrtc: Boolean(options.streamer_webrtc),
        moq: Boolean(options.streamer_moq),
    };
    if (has[want]) return want;
    if (want === 'moq') {
        console.warn('[MediaMtxDvrPlayer] proto=moq requires streamer_moq, falling back');
    } else if (want === 'webrtc') {
        console.warn('[MediaMtxDvrPlayer] proto=webrtc requires streamer_webrtc, falling back to HLS');
    }
    if (has.webrtc) return 'webrtc';
    if (has.hls) return 'hls';
    if (has.moq) return 'moq';
    return 'hls';
}

export function parseOptions(raw = {}) {
    const query = parseQuery(raw.query || '');
    const merged = { ...DEFAULTS, ...raw };

    merged.name = String(merged.name || query.name || '').replace(/^\/+|\/+$/g, '');
    merged.streamer_http = String(merged.streamer_http || query.streamer_http || '').replace(/\/+$/, '');
    merged.streamer_webrtc = String(merged.streamer_webrtc || query.streamer_webrtc || '').replace(/\/+$/, '');
    merged.streamer_moq = String(merged.streamer_moq || query.streamer_moq || '').replace(/\/+$/, '');
    merged.proto = parseProto(query.proto || merged.proto, 'webrtc');
    merged.locale = merged.locale || query.locale || 'ru';
    merged.dvr = parseBool(query.dvr, parseBool(merged.dvr, true));
    merged.debug = parseBool(query.debug, parseBool(merged.debug, false));
    merged.autoPlay = parseBool(query.autoplay ?? query.autoPlay, parseBool(merged.autoPlay, true));
    merged.muted = parseBool(query.muted, parseBool(merged.muted, true));
    merged.preview = parseBool(query.preview, parseBool(merged.preview, true));
    merged.hidecontrols = parseHideControls(query.hidecontrols ?? query.hideControls ?? merged.hidecontrols);
    merged.controls_min = parseControlsMin(
        query.controls_min ?? query.controlsMin ?? query.mincontrols ?? merged.controls_min,
        200,
    );
    merged.export = parseBool(query.export, parseBool(merged.export, false));
    merged.export_limit = parseExportLimit(query.export_limit ?? query.exportLimit ?? merged.export_limit);
    merged.fit = parseFit(query.fit ?? merged.fit, 'auto');
    merged.panzoom = parseBool(query.panzoom, parseBool(merged.panzoom, false));
    merged.fixedRange = parseBool(
        query.fixedRange ?? query.fixedrange ?? query.fixed_range,
        parseBool(merged.fixedRange, false),
    );
    merged.from = parseUnix(query.from) ?? parseUnix(merged.from);
    merged.to = parseUnix(query.to) ?? parseUnix(merged.to);
    merged.play_duration = parsePlayDuration(
        query.play_duration ?? query.playDuration ?? merged.play_duration,
    );
    merged.user = String(merged.user || query.user || query.username || '');
    merged.pass = String(merged.pass || query.pass || query.password || '');
    merged.tokenName = String(merged.tokenName || query.tokenName || 'token');
    merged.token = String(merged.token || query[merged.tokenName] || query.token || '');
    merged.title = merged.title || query.title || merged.name;
    merged.return_url = parseReturnUrl(query.return_url || query.returnUrl || merged.return_url);
    merged.centeredSeek = parseBool(
        query.centeredSeek ?? query.centered_seek ?? query.centeredseek,
        parseBool(merged.centeredSeek, false),
    );
    merged.event = parseBool(query.event, parseBool(merged.event, false));
    merged.query = String(raw.query || '').replace(/^\?/, '');

    merged.proto = resolveLiveProto(merged);

    return merged;
}

export function hasBasicAuth(options = {}) {
    return Boolean(String(options.user || '').trim());
}

/** Basic wins over Bearer when both are set (admin session vs embed token). */
export function authHeaders(options = {}) {
    const user = String(options.user || '').trim();
    const token = String(options.token || '').trim();
    if (user) {
        return { Authorization: `Basic ${btoa(`${user}:${options.pass ?? ''}`)}` };
    }
    if (token) {
        return { Authorization: `Bearer ${token}` };
    }
    return {};
}

export function authQuery(options) {
    if (hasBasicAuth(options) || !options.token) return '';
    const name = options.tokenName || 'token';
    return `${encodeURIComponent(name)}=${encodeURIComponent(options.token)}`;
}

export function authFetch(url, options, init = {}) {
    const headers = { ...authHeaders(options), ...(init.headers || {}) };
    return fetch(url, { cache: 'no-store', ...init, headers });
}

/** Native <video> / <a> cannot send Authorization; put Basic in the URL. */
export function withBasicCredentials(url, options) {
    const user = String(options?.user || '').trim();
    if (!user) return url;
    try {
        const parsed = new URL(url, typeof window !== 'undefined' ? window.location.href : undefined);
        parsed.username = user;
        parsed.password = String(options.pass ?? '');
        return parsed.toString();
    } catch (_e) {
        return url;
    }
}

export function streamBase(options) {
    return joinUrl(options.streamer_http, options.name);
}

export const MAX_ARCHIVE_DURATION = 3600;

export function liveUrl(options) {
    return withQuery(joinUrl(streamBase(options), 'index.m3u8'), authQuery(options));
}

function httpsBase(url) {
    const raw = String(url || '').replace(/\/+$/, '');
    if (!raw) return '';
    try {
        const parsed = new URL(raw, typeof window !== 'undefined' ? window.location.href : undefined);
        if (parsed.protocol === 'http:') parsed.protocol = 'https:';
        return parsed.toString().replace(/\/+$/, '');
    } catch (_e) {
        return raw.replace(/^http:/, 'https:');
    }
}

function absoluteUrl(path) {
    try {
        return new URL(path, window.location.href).toString();
    } catch (_e) {
        return path;
    }
}

/** MediaMTX read page: https://host:8892/{name}/ */
export function moqPageUrl(options) {
    const page = `${joinUrl(httpsBase(options.streamer_moq), options.name)}/`;
    return absoluteUrl(page);
}

export function liveMoqUrl(options) {
    const search = authQuery(options);
    return String(new URL('moq', moqPageUrl(options))) + (search ? `?${search}` : '');
}

export function moqFingerprintUrl(options) {
    return new URL('fingerprint', moqPageUrl(options)).toString();
}

export function archiveUrl(options, from, duration) {
    const dur = Math.max(1, Math.min(MAX_ARCHIVE_DURATION, Number(duration) || MAX_ARCHIVE_DURATION));
    return withQuery(
        joinUrl(streamBase(options), `index-${from}-${dur}.m3u8`),
        authQuery(options),
    );
}

export function archiveMp4Url(options, from, duration) {
    const limit = parseExportLimit(options.export_limit);
    const dur = Math.max(1, Math.min(limit, Math.floor(Number(duration) || 1)));
    return withQuery(
        joinUrl(streamBase(options), `archive-${Math.floor(from)}-${dur}.mp4`),
        authQuery(options),
    );
}

export function recordingStatusUrl(options) {
    return withQuery(joinUrl(streamBase(options), 'recording_status.json'), authQuery(options));
}

export const EVENTS_PAGE = 100;

export function eventsUrl(options, from, to, limit = EVENTS_PAGE, sort = 'desc') {
    const query = [
        `opened_at_gte=${Math.floor(from)}`,
        `opened_at_lt=${Math.floor(to)}`,
        `limit=${Math.max(1, Math.floor(limit) || EVENTS_PAGE)}`,
        `sort=${sort === 'asc' ? 'asc' : 'desc'}`,
        authQuery(options),
    ].filter(Boolean).join('&');
    return withQuery(joinUrl(streamBase(options), 'events.json'), query);
}

export function previewMp4Url(options, utc, kind = 'unix') {
    const ts = Math.floor(Number(utc) || 0);
    if (kind === 'date') {
        const date = new Date(ts * 1000);
        const stamp = [
            date.getUTCFullYear(),
            pad(date.getUTCMonth() + 1),
            pad(date.getUTCDate()),
            pad(date.getUTCHours()),
            pad(date.getUTCMinutes()),
            `${pad(date.getUTCSeconds())}.mp4`,
        ].join('/');
        return withQuery(joinUrl(streamBase(options), stamp), authQuery(options));
    }
    return withQuery(joinUrl(streamBase(options), `${ts}-preview.mp4`), authQuery(options));
}

export async function fetchEvents(options, from, to, limit = EVENTS_PAGE, sort = 'desc') {
    const page = Math.max(1, Math.floor(limit) || EVENTS_PAGE);
    const order = sort === 'asc' ? 'asc' : 'desc';
    const res = await authFetch(eventsUrl(options, from, to, page, order), options);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    const list = Array.isArray(data) ? data : data?.events;
    if (!Array.isArray(list)) return [];
    const events = list
        .map((item) => ({
            from: Math.floor(Number(item?.from)),
            duration: Math.max(0, Number(item?.duration) || 0),
            class: String(item?.class || ''),
            topic: String(item?.topic || ''),
            source: String(item?.source || ''),
        }))
        .filter((item) => Number.isFinite(item.from))
        .sort((a, b) => a.from - b.from || a.topic.localeCompare(b.topic));
    if (events.length <= page) return events;
    return order === 'asc' ? events.slice(0, page) : events.slice(-page);
}

export async function fetchRecordingRanges(options) {
    const url = recordingStatusUrl(options);
    const res = await authFetch(url, options);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    const entry = Array.isArray(data) ? data[0] : data;
    return Array.isArray(entry?.ranges) ? entry.ranges : [];
}

export function episodeFromOptions(options) {
    if (options.from == null) return null;
    const from = options.from;
    let duration;
    if (options.to != null && options.to > from) duration = options.to - from;
    else if (options.play_duration) duration = options.play_duration;
    else duration = MAX_ARCHIVE_DURATION;
    duration = Math.max(1, Math.min(MAX_ARCHIVE_DURATION, Math.floor(duration)));
    return { from, to: from + duration, duration };
}

export function archiveWindowFromOptions(options) {
    const episode = episodeFromOptions(options);
    if (!episode) return null;
    return {
        from: episode.from,
        duration: Math.max(1, episode.to - episode.from),
        to: episode.to,
    };
}
