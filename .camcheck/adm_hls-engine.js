import { isIOS } from './utils.js';

const LIVE_STALL_MS = 12_000;
const LIVE_WATCHDOG_MS = 4_000;
// Archive MP4 file boundaries often leave 0.5–1s PTS holes. hls.js default
// maxBufferHole is 0.1s, so playback stalls instead of jumping like VLC/Chrome.
const MAX_BUFFER_HOLE_S = 2;
const HLS_GAP_CONFIG = {
    maxBufferHole: MAX_BUFFER_HOLE_S,
    nudgeMaxRetry: 10,
    highBufferWatchdogPeriod: 1,
};

const LIVE_LOAD_POLICY = {
    maxTimeToFirstByteMs: 8_000,
    maxLoadTimeMs: 20_000,
    timeoutRetry: {
        maxNumRetry: 3,
        retryDelayMs: 1_000,
        maxRetryDelayMs: 4_000,
    },
    errorRetry: {
        maxNumRetry: 4,
        retryDelayMs: 1_000,
        maxRetryDelayMs: 8_000,
    },
};

export class HlsEngine {
    constructor({ video, debug = false }) {
        this.video = video;
        this.debug = debug;
        this.hls = null;
        this.retryTimer = null;
        this.watchdogTimer = null;
        this.lastProgressAt = 0;
        this._watchMediaTime = 0;
        this.mediaRecoveries = 0;
        this.currentUrl = null;
        this.currentOpts = null;
        this.nativeCleanups = [];
        this.handlers = {
            onMessage: () => {},
            onManifest: () => {},
            onFatal: () => {},
        };
    }

    on(handlers) {
        this.handlers = { ...this.handlers, ...handlers };
    }

    get liveSyncPosition() {
        if (this.hls && typeof this.hls.liveSyncPosition === 'number') {
            return this.hls.liveSyncPosition;
        }
        return null;
    }

    destroy() {
        this.#clearRetry();
        this.#stopWatchdog();
        this.#unbindNative();
        if (this.hls) {
            this.hls.destroy();
            this.hls = null;
        }
        this.currentUrl = null;
        this.currentOpts = null;
        this.#resetVideo();
    }

    load(url, { isLive = false, startTime = 0, retryPause = 5000, headers = {}, user = '', pass = '' } = {}) {
        this.#clearRetry();
        this.#stopWatchdog();
        this.#unbindNative();
        if (this.hls) {
            this.hls.destroy();
            this.hls = null;
        }
        this.#resetVideo();

        this.currentUrl = url;
        this.currentOpts = { isLive, startTime, retryPause, headers, user, pass };
        this.mediaRecoveries = 0;
        this.lastProgressAt = Date.now();
        this._watchMediaTime = this.video.currentTime || 0;

        if (typeof Hls !== 'undefined' && Hls.isSupported() && !isIOS()) {
            this.#loadWithHls(url, this.currentOpts);
        } else if (this.video.canPlayType('application/vnd.apple.mpegurl')) {
            this.#loadNative(url, this.currentOpts);
        } else {
            this.handlers.onFatal('no-hls');
            return;
        }

        if (isLive) {
            this.#bindNative('ended', () => {
                this.#scheduleReload(url, this.currentOpts, 'networkRetry');
            });
            this.#startWatchdog(url, this.currentOpts);
        }
    }

    #loadWithHls(url, { isLive, startTime, retryPause, headers }) {
        const xhrSetup = hlsXhrSetup(headers);
        const config = isLive
            ? {
                enableWorker: true,
                autoStartLoad: true,
                lowLatencyMode: true,
                backBufferLength: 0,
                liveSyncDurationCount: 3,
                liveMaxLatencyDurationCount: 6,
                maxLiveSyncPlaybackRate: 1.5,
                liveDurationInfinity: true,
                manifestLoadPolicy: { default: LIVE_LOAD_POLICY },
                playlistLoadPolicy: { default: LIVE_LOAD_POLICY },
                fragLoadPolicy: { default: LIVE_LOAD_POLICY },
                debug: this.debug,
                xhrSetup,
                ...HLS_GAP_CONFIG,
            }
            : {
                enableWorker: true,
                autoStartLoad: true,
                lowLatencyMode: false,
                backBufferLength: 90,
                debug: this.debug,
                xhrSetup,
                ...HLS_GAP_CONFIG,
            };

        this.hls = new Hls(config);
        const sourceUrl = isLive ? cacheBust(url) : url;

        this.hls.on(Hls.Events.ERROR, (_event, data) => {
            if (data?.details === Hls.ErrorDetails.BUFFER_STALLED_ERROR) {
                this.#skipBufferHole();
                if (!data.fatal) return;
            }
            if (!data?.fatal) return;
            const opts = this.currentOpts || { isLive, startTime, retryPause };
            if (data.type === Hls.ErrorTypes.NETWORK_ERROR) {
                this.#recoverLiveOrRetry(url, opts, 'networkRetry', () => {
                    this.hls.startLoad();
                });
                return;
            }
            if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
                this.handlers.onMessage('mediaRecover');
                if (isLive && this.mediaRecoveries >= 1) {
                    this.#scheduleReload(url, opts, 'fatalReload');
                    return;
                }
                this.mediaRecoveries += 1;
                try {
                    this.hls.recoverMediaError();
                } catch (_e) {
                    this.#scheduleReload(url, opts, 'fatalReload');
                }
                return;
            }
            this.#scheduleReload(url, opts, 'fatalReload');
        });

        let manifestReported = false;
        const reportManifest = (details) => {
            if (manifestReported || !details) return;
            manifestReported = true;
            this.#markProgress(true);
            const programFrag = details.fragments?.find((item) => item.programDateTime);
            this.handlers.onManifest({
                live: Boolean(details.live),
                duration: details.totalduration || null,
                programStartTime: programFrag ? new Date(programFrag.programDateTime) : null,
                native: false,
                fragments: (details.fragments || []).map((item) => ({
                    start: item.start,
                    duration: item.duration,
                    programDateTime: item.programDateTime || null,
                })),
            });
            if (startTime > 0 && !details.live) {
                this.video.currentTime = startTime;
            }
        };

        this.hls.on(Hls.Events.LEVEL_LOADED, (_event, data) => reportManifest(data.details));
        this.hls.on(Hls.Events.MANIFEST_LOADED, (_event, data) => {
            reportManifest(data?.levels?.[0]?.details);
        });
        this.hls.on(Hls.Events.FRAG_LOADED, () => this.#markProgress(true));
        this.hls.on(Hls.Events.FRAG_CHANGED, () => this.#markProgress(true));

        this.hls.attachMedia(this.video);
        this.hls.on(Hls.Events.MEDIA_ATTACHED, () => {
            this.hls.loadSource(sourceUrl);
        });
    }

    #loadNative(url, { isLive, startTime, retryPause, user, pass }) {
        let sourceUrl = withUrlCredentials(url, user, pass);
        if (isLive) sourceUrl = cacheBust(sourceUrl);
        const onMeta = () => {
            this.#markProgress(true);
            const live = !this.video.duration || !isFinite(this.video.duration);
            this.handlers.onManifest({
                live,
                duration: live ? null : this.video.duration,
                programStartTime: null,
                native: true,
            });
            if (startTime > 0 && !live) {
                this.video.currentTime = startTime;
            }
        };
        const onProgress = () => this.#markProgress(true);
        const onError = () => {
            this.#scheduleReload(url, this.currentOpts || { isLive, startTime, retryPause }, 'networkRetry');
        };

        this.#bindNative('loadedmetadata', onMeta);
        this.#bindNative('timeupdate', onProgress);
        this.#bindNative('playing', onProgress);
        this.#bindNative('error', onError);
        this.video.src = sourceUrl;
    }

    #recoverLiveOrRetry(url, opts, messageKey, recover) {
        if (opts.isLive) {
            this.#scheduleReload(url, opts, messageKey);
            return;
        }
        this.handlers.onMessage(messageKey);
        try {
            recover();
        } catch (_e) {
            this.#scheduleReload(url, opts, messageKey);
        }
    }

    #scheduleReload(url, opts, messageKey) {
        if (this.retryTimer) return;
        this.handlers.onMessage(messageKey);
        this.#stopWatchdog();
        if (this.hls) {
            try { this.hls.stopLoad(); } catch (_e) { /* ignore */ }
        }
        this.retryTimer = setTimeout(() => {
            this.retryTimer = null;
            this.load(url, opts);
        }, opts.retryPause || 5000);
    }

    #startWatchdog(url, opts) {
        this.#stopWatchdog();
        this.watchdogTimer = setInterval(() => {
            if (this.retryTimer) return;
            if (this.video.paused) return;
            const mediaTime = this.video.currentTime;
            if (Number.isFinite(mediaTime) && mediaTime !== this._watchMediaTime) {
                this._watchMediaTime = mediaTime;
                this.#markProgress(false);
            }
            if (Date.now() - this.lastProgressAt < LIVE_STALL_MS) return;
            this.#scheduleReload(url, opts, 'networkRetry');
        }, LIVE_WATCHDOG_MS);
    }

    #stopWatchdog() {
        if (!this.watchdogTimer) return;
        clearInterval(this.watchdogTimer);
        this.watchdogTimer = null;
    }

    #skipBufferHole() {
        const media = this.video;
        if (!media || media.ended) return;
        const time = media.currentTime;
        if (!Number.isFinite(time)) return;
        const ranges = media.buffered;
        for (let i = 0; i < ranges.length; i++) {
            const start = ranges.start(i);
            const end = ranges.end(i);
            if (time >= start && time < end - 0.04) return;
            if (start > time && start - time <= MAX_BUFFER_HOLE_S) {
                media.currentTime = start + 0.04;
                this.#markProgress(true);
                return;
            }
        }
    }

    #markProgress(resetRecoveries) {
        this.lastProgressAt = Date.now();
        if (resetRecoveries) this.mediaRecoveries = 0;
    }

    #clearRetry() {
        if (!this.retryTimer) return;
        clearTimeout(this.retryTimer);
        this.retryTimer = null;
    }

    #bindNative(event, handler) {
        this.video.addEventListener(event, handler);
        this.nativeCleanups.push(() => this.video.removeEventListener(event, handler));
    }

    #unbindNative() {
        this.nativeCleanups.forEach((fn) => fn());
        this.nativeCleanups = [];
    }

    #resetVideo() {
        this.video.pause();
        if (this.video.srcObject) this.video.srcObject = null;
        this.video.removeAttribute('src');
        this.video.load();
    }
}

function hlsXhrSetup(headers) {
    if (!headers || !Object.keys(headers).length) return undefined;
    return (xhr) => {
        for (const [key, value] of Object.entries(headers)) {
            if (value) xhr.setRequestHeader(key, value);
        }
    };
}

function withUrlCredentials(url, user, pass) {
    const name = String(user || '').trim();
    if (!name) return url;
    try {
        const parsed = new URL(url, window.location.href);
        parsed.username = name;
        parsed.password = String(pass ?? '');
        return parsed.toString();
    } catch (_e) {
        return url;
    }
}

function cacheBust(url) {
    try {
        const parsed = new URL(url, window.location.href);
        parsed.searchParams.set('_r', String(Date.now()));
        return parsed.toString();
    } catch (_e) {
        const join = url.includes('?') ? '&' : '?';
        return `${url}${join}_r=${Date.now()}`;
    }
}
