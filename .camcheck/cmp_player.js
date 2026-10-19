import { archiveMp4Url, archiveUrl, authFetch, authHeaders, authQuery, episodeFromOptions, EVENTS_PAGE, fetchEvents, fetchRecordingRanges, hasBasicAuth, liveUrl, MAX_ARCHIVE_DURATION, moqPageUrl, previewMp4Url, resolveLiveProto, streamBase } from './api.js?v=13';
import { DatePicker, buildAvailableDays } from './datepicker.js';
import { HlsEngine } from './hls-engine.js?v=5';
import { MoqEngine } from './moq-engine.js?v=20';
import { WebrtcEngine } from './webrtc-engine.js?v=10';
import { t } from './i18n.js?v=8';
import { bindElements, ICON_BACK, ICON_PAUSE, ICON_PLAY, ICON_VOLUME, ICON_VOLUME_OFF, renderTemplate } from './template.js?v=14';
import { Timeline, growOpenRange, snapToRange } from './timeline.js?v=6';
import {
    debugLog,
    formatDateTime,
    joinUrl,
    loadScript,
    nextLocalMidnightTs,
    pad,
    startOfLocalDayTs,
    withQuery,
} from './utils.js?v=4';

const PLAYBACK_RATES = [0.25, 0.5, 1, 2, 4, 8, 16];
const RANGES_TTL_MS = 10_000;
const RECORDING_ONGOING_SLACK = 180;

export class MediaMtxPlayer {
    constructor(options, container) {
        this.options = normalizeLiveOptions(options);
        this.container = container;
        this.els = null;
        this.engine = null;
        this.webrtc = null;
        this.moq = null;
        this.timeline = null;
        this.datepicker = null;
        this.isLive = true;
        this.playbackRate = 1;
        this.programStartTime = null;
        this.archiveFrom = null;
        this.archiveDuration = null;
        this.pendingStartTime = 0;
        this.recordingRanges = [];
        this._recordingOngoing = false;
        this._rangesFetchedAt = 0;
        this._rangesFetch = null;
        this.unsubscribers = [];
        this.volume = 1;
        this.targetUtc = null;
        this.userSeeking = false;
        this.seekToken = 0;
        this._advancing = false;
        this._thumbUtc = null;
        this._thumbKind = 'unix';
        this._thumbTimer = null;
        this._thumbSrc = '';
        this._thumbBlob = null;
        this._hideControlsTimer = null;
        this.exportActive = false;
        this.exportFrom = null;
        this.exportTo = null;
        this._exportDownloading = false;
        this._exportDownloadTimer = null;
        this._videoZoom = 1;
        this._videoPanX = 0;
        this._videoPanY = 0;
        this.episodeFrom = null;
        this.episodeTo = null;
        this._playedMs = 0;
        this._lastMediaTime = null;
        this._limitStopped = false;
        this._playlistRanges = [];
        this._playlistStartSnapped = false;
        this._liveStarted = false;
        this._eventsOpen = false;
        this._eventsTimer = null;
        this._eventsWatch = null;
        this._eventsToken = 0;
        this._eventsLoadedFrom = null;
        this._eventsLoadedTo = null;
        this._eventsWatchKey = '';
        this._eventsHasOlder = false;
        this._eventsHasNewer = false;
        this._eventsOlderFloor = null;
        this._eventsNewerFloor = null;
        this._eventsEmptyOlder = 0;
        this._eventsEmptyNewer = 0;
        this._eventsPaging = null;
        this._eventsScrollLock = false;
        this._eventsLeftTop = false;
        this._eventPreviewObserver = null;
        this._eventPreviewJobs = new Map();
        this._eventPreviewVisible = new Set();
        this._eventPreviewSettle = null;
    }

    async mount() {
        this.container.classList.add('mtx-dvr', 'dvrContainer', `fit-${this.options.fit || 'auto'}`, 'no-chrome-anim');
        if (this.options.centeredSeek) this.container.classList.add('is-centered-seek');
        if (this.#controlsMin()) {
            this.container.classList.add('controls-hidden', 'is-compact');
            this._compactControls = true;
        }
        this.container.innerHTML = renderTemplate(this.options.locale);
        this.els = bindElements(this.container);
        this.els.video.muted = this.options.muted;
        this.els.video.autoplay = this.options.autoPlay;
        this.els.video.playsInline = true;
        this._protoLogOpen = false;
        this.els.video.volume = this.volume;
        this.#syncMuteIcon();
        this.els['proto-log-btn']?.addEventListener('click', () => {
            this._protoLogOpen = !this._protoLogOpen;
            this.#syncProtoLogUi();
        });
        this.els['proto-log-toggle']?.addEventListener('click', () => {
            this._protoLogOpen = false;
            this.#syncProtoLogUi();
        });

        this.engine = new HlsEngine({ video: this.els.video, debug: this.options.debug });
        this.engine.on({
            onMessage: (key) => this.setMessage(t(this.options.locale, key)),
            onManifest: (info) => this.#onManifest(info),
            onFatal: () => this.setMessage(t(this.options.locale, 'noHls'), true),
        });
        this.webrtc = new WebrtcEngine({ video: this.els.video, debug: this.options.debug });
        this.webrtc.on({
            onMessage: (key) => {
                this.setMessage(t(this.options.locale, key));
                if (this.#useWebrtc()) this.#protoLog(`msg ${key}`);
            },
            onManifest: (info) => this.#onManifest(info),
            onFatal: (reason) => {
                if (this.#fallbackWebrtcToHls(reason)) return;
                this.setMessage(t(this.options.locale, 'noWebrtc'), true);
                this.#protoLog(`fatal ${reason || 'no-webrtc'}`);
            },
            onDiag: (info) => this.#renderProtoDebug(info),
        });
        this.moq = new MoqEngine({
            stage: this.els['moq-stage'],
            video: this.els.video,
            debug: this.options.debug,
        });
        this.moq.on({
            onMessage: (msg) => {
                this.setMessage(msg);
                if (msg && this.#useMoq()) this.#protoLog(`msg ${msg}`);
            },
            onManifest: (info) => this.#onManifest(info),
            onFatal: () => {
                this.setMessage(t(this.options.locale, 'noMoq'), true);
                this.#protoLog('fatal no WebTransport/VideoDecoder');
            },
            onDiag: (info) => this.#renderProtoDebug(info),
            onAudioMuted: (muted) => {
                if (muted) this.els.video.muted = true;
                else if (this.els.video.volume > 0) this.els.video.muted = false;
                this.#syncMuteIcon();
            },
            onPlaying: () => {
                this.els.play.innerHTML = ICON_PAUSE;
                this.els.play.title = t(this.options.locale, 'pause');
            },
            onPaused: () => {
                this.els.play.innerHTML = ICON_PLAY;
                this.els.play.title = t(this.options.locale, 'play');
            },
        });
        this.#syncProtoUi();

        this.timeline = new Timeline(this.els, { centeredSeek: !!this.options.centeredSeek });
        this.timeline.bind({
            onPreview: (utc) => this.#previewSeek(utc),
            onCommit: (utc) => this.seekToUtc(utc),
            onViewChange: () => {
                this.#refreshRanges();
            },
            onHover: (utc, x) => this.#showThumb(utc, x),
            onHoverEnd: () => this.#hideThumb(),
        });

        this.datepicker = new DatePicker(this.els.datepicker, this.options.locale);
        this.datepicker.onSelect = (dayTs) => this.openArchiveDay(dayTs);

        this.#bindUi();
        this.#bindCompactControls();
        this.#bindControlsVisibility();
        this.container.classList.add('is-ready');
        requestAnimationFrame(() => {
            requestAnimationFrame(() => this.container.classList.remove('no-chrome-anim'));
        });
        this.#bindExport();
        this.#bindEvents();
        this.#bindEventsLayout();
        this.#bindVideoZoom();
        this.#setupBackButton();
        await this.#ensureHls();
        await this.#refreshRanges();
        this.#applyEpisodeOptions();
        this.#startEventsWatch();
    }

    #applyEpisodeOptions() {
        const episode = episodeFromOptions(this.options);
        if (!episode) {
            if (this.options.autoPlay) {
                this.goLive();
            } else {
                this.#armLiveUi();
                this.#showLoader(false);
                this.setMessage(t(this.options.locale, 'clickPlay'));
            }
            return;
        }
        this.episodeFrom = episode.from;
        this.episodeTo = episode.to;
        this.timeline.lockRange(episode.from, episode.to);
        if (this.options.fixedRange) {
            this.container.classList.add('is-fixed-range');
        }
        const start = this.#firstRecordingAtOrAfter(episode.from, episode.to);
        this.openArchive(start, Math.max(1, episode.to - start));
    }

    play() {
        if (this._limitStopped) {
            this.#clearPlaybackLimit();
            if (this.episodeFrom != null && this.episodeTo != null && this.#playheadUtc() >= this.episodeTo - 1) {
                this.seekToUtc(this.#firstRecordingAtOrAfter(this.episodeFrom, this.episodeTo));
                return this.els.video.play();
            }
        }
        if (this.isLive && !this._liveStarted) {
            this.els.video.autoplay = true;
            this.goLive();
            return Promise.resolve();
        }
        if (this.#useMoq()) {
            this.moq.play();
            return Promise.resolve();
        }
        return this.els.video.play();
    }

    stop() {
        this.webrtc?.destroy();
        this.moq?.destroy();
        this.engine?.destroy();
    }

    restart() {
        if (this.isLive) this.goLive();
        else this.openArchive(this.archiveFrom, this.archiveDuration);
    }

    changeLanguage(locale) {
        this.options.locale = locale;
        this.destroy();
        return this.mount();
    }

    destroy() {
        this.unsubscribers.forEach((fn) => fn());
        this.unsubscribers = [];
        clearTimeout(this._thumbTimer);
        this._thumbTimer = null;
        clearTimeout(this._hideControlsTimer);
        this._hideControlsTimer = null;
        clearTimeout(this._eventsTimer);
        this._eventsTimer = null;
        clearTimeout(this._eventsWatch);
        this._eventsWatch = null;
        this._eventsToken += 1;
        this.#disposeEventPreviews();
        this.#resetExportDownload();
        this.#revokeThumbBlob();
        if (this.els?.['thumb-video']) {
            this.els['thumb-video'].removeAttribute('src');
            this.els['thumb-video'].load();
        }
        this.datepicker?.destroy();
        this.#stopMoqClock();
        this.webrtc?.destroy();
        this.webrtc = null;
        this.moq?.destroy();
        this.moq = null;
        this.engine?.destroy();
        this.engine = null;
        this.container.innerHTML = '';
        this.container.classList.remove('mtx-dvr', 'dvrContainer', 'controls-hidden', 'is-compact', 'is-ready', 'no-chrome-anim', 'is-fixed-range', 'is-centered-seek', 'has-back', 'has-events', 'events-open', 'events-stack', 'is-moq');
    }

    goLive() {
        if (!this.#armLiveUi()) return;
        this._liveStarted = true;
        this.#showLoader(true);
        this.#loadLive();
    }

    #armLiveUi() {
        if (this.options.fixedRange && this.episodeFrom != null) return false;
        this.#clearEpisodeBounds();
        this.isLive = true;
        this.archiveFrom = null;
        this.archiveDuration = null;
        this.pendingStartTime = 0;
        this.targetUtc = null;
        this.datepicker.selectedDayStart = null;
        this.datepicker.hide();
        const now = Math.floor(Date.now() / 1000);
        const liveLead = this.options.centeredSeek ? 0.5 : 0.88;
        this.timeline.setView(now - 3600 * liveLead, 3600);
        this.#setModeUi();
        this.#refreshRanges();
        return true;
    }

    openArchive(from, duration = MAX_ARCHIVE_DURATION, startOffset = 0, { resetView = true } = {}) {
        if (!streamBase(this.options)) {
            this.setMessage(t(this.options.locale, 'noStream'), true);
            return;
        }
        const start = Math.floor(from);
        const requested = Math.max(1, Math.round(duration));
        let clipDuration = Math.min(MAX_ARCHIVE_DURATION, requested);
        if (this.episodeFrom != null) {
            clipDuration = Math.max(1, clipDuration);
        } else {
            clipDuration = Math.max(120, clipDuration);
        }
        if (this.episodeTo != null) {
            clipDuration = Math.max(1, Math.min(clipDuration, this.episodeTo - start));
        }
        this.isLive = false;
        this.archiveFrom = start;
        this.archiveDuration = clipDuration;
        this.pendingStartTime = Math.max(0, startOffset);
        this.datepicker.selectedDayStart = startOfLocalDayTs(new Date(start * 1000));
        this.datepicker.hide();
        if (resetView && this.timeline.lockFrom == null) {
            if (this.options.centeredSeek) {
                this.timeline.setView(start - clipDuration / 2, clipDuration);
            } else {
                this.timeline.setView(start, clipDuration);
            }
        }
        const utc = this.archiveFrom + this.pendingStartTime;
        this.#pinPlayhead(utc);
        this.#setModeUi();
        this._advancing = false;
        this.#beginSeekLoad(() => this.#load(archiveUrl(this.options, this.archiveFrom, clipDuration), 0));
    }

    async openArchiveDay(dayTs) {
        if (this.options.fixedRange) return;
        this.#clearEpisodeBounds();
        this.timeline.setView(dayTs, 86400);
        await this.#refreshRanges({ force: true });
        const start = this.#firstRecordingAtOrAfter(dayTs, dayTs + 86400);
        this.openArchive(start, MAX_ARCHIVE_DURATION, 0, { resetView: false });
    }

    #firstRecordingAtOrAfter(from, until = from + 86400) {
        const ranges = this._playlistRanges.length ? this._playlistRanges : this.recordingRanges;
        let best = null;
        for (const range of ranges || []) {
            const start = Number(range.from) || 0;
            const stop = start + (Number(range.duration) || 0);
            if (stop <= from || start >= until) continue;
            const utc = Math.max(from, start);
            if (best == null || utc < best) best = utc;
        }
        return best ?? from;
    }

    #previewSeek(utc) {
        utc = this.#clampToEpisode(snapToRange(utc, this.#seekRanges()));
        this.isLive = false;
        this.#pinPlayhead(utc);
        this.#setModeUi();
    }

    #showThumb(utc, clientX) {
        if (!this.options.preview || !this.options.dvr || !this.els['thumb-box']) return;
        const now = Math.floor(Date.now() / 1000);
        if (utc > now + 2) {
            this.#hideThumb();
            return;
        }

        const root = this.container.getBoundingClientRect();
        const seek = this.els.seek.getBoundingClientRect();
        const width = 213;
        const x = Math.max(width / 2 + 8, Math.min(root.width - width / 2 - 8, clientX - root.left));
        this.els['thumb-box'].style.left = `${x}px`;
        this.els['thumb-box'].style.bottom = `${root.bottom - seek.top + 8}px`;
        this.els['thumb-box'].classList.add('is-visible');
        this.els['thumb-caption'].textContent = formatDateTime(new Date(utc * 1000));

        const step = Math.max(1, Math.round(this.timeline.secondsPerPixel()));
        const snapped = utc - (utc % step);
        if (snapped !== this._thumbUtc || !this._thumbSrc) {
            this._thumbUtc = snapped;
            clearTimeout(this._thumbTimer);
            this._thumbTimer = setTimeout(() => this.#loadThumb(snapped), 90);
        }
    }

    #loadThumb(utc) {
        const video = this.els['thumb-video'];
        if (!video) return;
        const url = previewMp4Url(this.options, utc, this._thumbKind);
        if (this._thumbSrc === url) return;
        this._thumbSrc = url;
        this.els['thumb-loader'].classList.add('is-visible');
        this.#revokeThumbBlob();
        if (hasBasicAuth(this.options)) {
            authFetch(url, this.options)
                .then((res) => {
                    if (!res.ok || this._thumbSrc !== url) return null;
                    return res.blob();
                })
                .then((blob) => {
                    if (!blob || this._thumbSrc !== url) return;
                    this._thumbBlob = URL.createObjectURL(blob);
                    video.src = this._thumbBlob;
                    video.play().catch(() => {});
                })
                .catch(() => {});
            return;
        }
        video.src = url;
        video.play().catch(() => {});
    }

    #revokeThumbBlob() {
        if (!this._thumbBlob) return;
        URL.revokeObjectURL(this._thumbBlob);
        this._thumbBlob = null;
    }

    #hideThumb() {
        clearTimeout(this._thumbTimer);
        this._thumbTimer = null;
        this.els['thumb-box']?.classList.remove('is-visible');
    }

    #clearEpisodeBounds() {
        this.episodeFrom = null;
        this.episodeTo = null;
        this._playlistRanges = [];
        this._playlistStartSnapped = false;
        this.timeline.clearLock();
        if (!this.options.fixedRange) this.#applyRecordingRanges();
    }

    #clampToEpisode(utc) {
        if (utc == null || !Number.isFinite(utc)) return utc;
        if (!this.options.fixedRange) return utc;
        if (this.episodeFrom != null && utc < this.episodeFrom) utc = this.episodeFrom;
        if (this.episodeTo != null && utc > this.episodeTo) utc = this.episodeTo;
        return utc;
    }

    #tickPlayDuration() {
        if (!this.options.play_duration) return;
        const video = this.els.video;
        if (!video || video.paused || video.ended) {
            this._lastMediaTime = video?.currentTime ?? this._lastMediaTime;
            return;
        }
        const time = video.currentTime;
        if (this._lastMediaTime != null && time > this._lastMediaTime) {
            this._playedMs += (time - this._lastMediaTime) * 1000;
        }
        this._lastMediaTime = time;
    }

    #clearPlaybackLimit() {
        this._limitStopped = false;
        this._playedMs = 0;
        this._lastMediaTime = null;
    }

    #stopAtLimit(utc) {
        if (this._limitStopped) return true;
        this._limitStopped = true;
        this.#finishSeek();
        const at = this.#clampToEpisode(utc ?? this.#playheadUtc());
        this.timeline.setPlayhead(at);
        this.els.timestamp.textContent = formatDateTime(new Date(at * 1000));
        try { this.els.video.pause(); } catch (_e) { /* ignore */ }
        return true;
    }

    #enforcePlaybackLimits(utc) {
        utc = this.#clampToEpisode(utc);
        this.#tickPlayDuration();
        if (this.episodeTo != null && utc >= this.episodeTo) return this.#stopAtLimit(this.episodeTo);
        if (this.options.play_duration && this._playedMs / 1000 >= this.options.play_duration) {
            return this.#stopAtLimit(utc);
        }
        return false;
    }

    async seekToUtc(utc) {
        if (!this.options.fixedRange && this.episodeFrom != null && this.episodeTo != null
            && (utc < this.episodeFrom || utc > this.episodeTo)) {
            this.#clearEpisodeBounds();
        }
        utc = this.#clampToEpisode(utc);
        if (utc == null) return;
        const now = Math.floor(Date.now() / 1000);
        if (!this.episodeTo && utc >= now - 12) {
            this.goLive();
            return;
        }

        if (!this.options.fixedRange) {
            this.#showLoader(true);
            await this.#refreshRanges({ force: true });
        }

        utc = snapToRange(utc, this.#seekRanges());
        utc = this.#clampToEpisode(utc);
        if (utc == null) return;

        this.#clearPlaybackLimit();
        this.#previewSeek(utc);

        const origin = this.#mediaOriginUtc();
        const length = this.#mediaLength();
        const offset = origin != null ? utc - origin : null;
        const inCurrentClip = !this.isLive && origin != null && offset != null && length != null
            && offset >= -1 && offset <= length + 2;

        if (inCurrentClip || this.options.fixedRange) {
            const time = origin == null
                ? 0
                : Math.max(0, length != null ? Math.min(offset, length) : Math.max(0, offset));
            this.#beginSeekLoad(() => {
                try { this.els.video.currentTime = time; } catch (_e) { /* ignore */ }
                this.els.video.play().catch(() => {});
                this.#finishSeek();
            });
            return;
        }

        this.#loadClipFor(utc);
    }

    #mediaOriginUtc() {
        if (this.programStartTime) return Math.floor(this.programStartTime.getTime() / 1000);
        return this.archiveFrom;
    }

    #mediaLength() {
        const duration = this.els.video.duration;
        if (Number.isFinite(duration) && duration > 0) return duration;
        if (this.archiveDuration) return this.archiveDuration;
        if (this._playlistRanges.length) {
            const first = this._playlistRanges[0];
            const last = this._playlistRanges[this._playlistRanges.length - 1];
            return (last.from + last.duration) - first.from;
        }
        return null;
    }

    #clipWindow(utc) {
        if (this.episodeFrom != null && this.episodeTo != null) {
            const span = Math.max(1, this.episodeTo - this.episodeFrom);
            const duration = Math.max(1, Math.min(MAX_ARCHIVE_DURATION, span));
            let from = Math.floor(utc - duration * 0.15);
            from = Math.max(this.episodeFrom, from);
            if (from + duration > this.episodeTo) from = this.episodeTo - duration;
            from = Math.max(this.episodeFrom, Math.floor(from));
            return { from, duration: Math.max(1, Math.min(duration, this.episodeTo - from)) };
        }
        const viewDur = this.timeline.viewDuration;
        const duration = Math.max(120, Math.min(MAX_ARCHIVE_DURATION, Math.round(viewDur)));
        let from = Math.floor(this.timeline.viewFrom);
        if (viewDur > MAX_ARCHIVE_DURATION || utc < from || utc > from + duration) {
            from = Math.floor(utc - duration * 0.15);
        }
        return { from, duration };
    }

    #clipCovers(utc) {
        if (this.isLive || this.archiveFrom == null || this.archiveDuration == null) return false;
        return utc >= this.archiveFrom && utc <= this.archiveFrom + this.archiveDuration - 2;
    }

    #loadClipFor(utc, { onlyIfNeeded = false } = {}) {
        const { from, duration } = this.#clipWindow(utc);
        if (onlyIfNeeded && this.#clipCovers(utc)) {
            const similar = Math.abs(this.archiveDuration - duration) < Math.max(30, duration * 0.25);
            if (similar) return;
        }
        this.openArchive(from, duration, Math.max(0, utc - from), { resetView: false });
    }

    #maybeAdvanceClip(utc) {
        if (this.#enforcePlaybackLimits(utc)) return;
        if (this.options.fixedRange || this.episodeTo != null) return;
        if (this.isLive || this.userSeeking || this.targetUtc != null || this._advancing) return;
        if (this.archiveFrom == null || this.archiveDuration == null) return;
        if (this.archiveDuration <= 15) return;
        const remain = this.archiveFrom + this.archiveDuration - utc;
        if (remain > 15) return;
        this._advancing = true;
        const { duration } = this.#clipWindow(utc);
        this.openArchive(utc, duration, 0, { resetView: false });
    }

    setMessage(text, isError = false) {
        if (text) {
            this.els.message.textContent = text;
            this.els.message.style.display = 'block';
        } else {
            this.els.message.style.display = 'none';
        }
        if (isError) debugLog(true, text);
    }

    #protoLog(line) {
        const el = this.els?.['proto-debug'];
        if (!el) return;
        const stamp = new Date().toISOString().slice(11, 23);
        this._protoLogLines = this._protoLogLines || [];
        String(line).split('\n').forEach((part) => {
            if (part) this._protoLogLines.push(`${stamp} ${part}`);
        });
        if (this._protoLogLines.length > 100) {
            this._protoLogLines.splice(0, this._protoLogLines.length - 100);
        }
        el.textContent = this._protoLogLines.join('\n');
        el.scrollTop = el.scrollHeight;
        this.#syncProtoLogUi();
    }

    #protoLogMode() {
        if (this.#useMoq()) return 'moq';
        if (this.#useWebrtc()) return 'webrtc';
        return '';
    }

    #syncProtoLogUi() {
        const panel = this.els?.['proto-log'];
        const btn = this.els?.['proto-log-btn'];
        const head = this.els?.['proto-log-toggle'];
        const mode = this.#protoLogMode();
        const label = mode === 'moq' ? 'MoQ log' : mode === 'webrtc' ? 'WebRTC log' : 'log';
        const show = Boolean(mode && this._protoLogOpen);
        if (btn) {
            btn.hidden = !mode;
            btn.title = label;
            btn.classList.toggle('is-active', show);
        }
        if (head) head.textContent = label;
        if (!panel) return;
        panel.hidden = !show;
        panel.classList.toggle('is-on', show);
        if (!mode) this._protoLogOpen = false;
    }

    #logMoqEnv() {
        const stage = this.els?.['moq-stage'];
        const rect = stage?.getBoundingClientRect();
        this._protoLogLines = [];
        this.#protoLog([
            `WT ${typeof WebTransport}  VD ${typeof VideoDecoder}  secure ${window.isSecureContext}`,
            `page ${location.href}`,
            `size ${window.innerWidth}x${window.innerHeight}  stage ${Math.round(rect?.width || 0)}x${Math.round(rect?.height || 0)}`,
            `ua ${navigator.userAgent}`,
            `streamer_moq ${this.options.streamer_moq || '-'}  proto ${this.options.proto}`,
            `pageUrl ${moqPageUrl(this.options)}`,
        ].join('\n'));
    }

    #logWebrtcEnv() {
        const rect = this.els?.video?.getBoundingClientRect();
        this._protoLogLines = [];
        this.#protoLog([
            `PC ${typeof RTCPeerConnection}  secure ${window.isSecureContext}`,
            `page ${location.href}`,
            `size ${window.innerWidth}x${window.innerHeight}  video ${Math.round(rect?.width || 0)}x${Math.round(rect?.height || 0)}`,
            `ua ${navigator.userAgent}`,
            `streamer_webrtc ${this.options.streamer_webrtc || '-'}  proto ${this.options.proto}`,
            `whep ${liveWebrtcUrl(this.options)}`,
        ].join('\n'));
    }

    #renderProtoDebug(info) {
        const extra = info?.extra || {};
        const bits = [
            info?.event || extra.event || info?.step,
            extra.url && `url=${extra.url}`,
            extra.fingerprint && `fp=${extra.fingerprint}`,
            extra.origin && `origin=${extra.origin}`,
            extra.secure != null && `secure=${extra.secure}`,
            extra.auth && `auth=${extra.auth}`,
            extra.user != null && `user=${extra.user}`,
            extra.token != null && `token=${extra.token}`,
            extra.mirrored != null && `mirror=${extra.mirrored}`,
            extra.href && extra.href,
            extra.status != null && `status=${extra.status}`,
            extra.session && `session=${extra.session}`,
            extra.video && `video=${extra.video}`,
            extra.audio && `audio=${extra.audio}`,
            extra.browser && `browser=${extra.browser}`,
            extra.codecs && `codec=${extra.codecs}`,
            extra.codec && `codec=${extra.codec}`,
            extra.renderer && `draw=${extra.renderer}`,
            extra.bytes != null && `bytes=${extra.bytes}`,
            extra.count != null && `count=${extra.count}`,
            extra.iceServers != null && `ice=${extra.iceServers}`,
            extra.l16Local != null && `l16Local=${extra.l16Local}`,
            extra.muteAudio != null && `muteAudio=${extra.muteAudio}`,
            extra.l16 != null && `l16=${extra.l16}`,
            extra.kind && `kind=${extra.kind}`,
            extra.id && `id=${extra.id}`,
            extra.streams != null && `streams=${extra.streams}`,
            extra.videoW != null && `size=${extra.videoW}x${extra.videoH}`,
            (extra.videoW != null || extra.n != null) && extra.n != null && `frames=${extra.n} bitmap=${extra.bitmap} css=${extra.css}`,
            extra.muted != null && `trackMuted=${extra.muted}`,
            extra.enabled != null && `enabled=${extra.enabled}`,
            extra.ready && `ready=${extra.ready}`,
            extra.paused != null && `paused=${extra.paused}`,
            extra.tries != null && `tries=${extra.tries}`,
            extra.stats && extra.stats,
            extra.ua && `ua=${extra.ua}`,
            extra.desc != null && `desc=${extra.desc}`,
            extra.note && extra.note,
            extra.state && `state=${extra.state}`,
            extra.retry != null && `retry=${extra.retry}`,
            extra.message || extra.last,
        ].filter((part) => part !== false && part != null && part !== '');
        this.#protoLog(bits.join(' '));
    }

    #pinPlayhead(utc) {
        this.targetUtc = utc;
        this.timeline.setPlayhead(utc);
        this.els.timestamp.textContent = formatDateTime(new Date(utc * 1000));
    }

    #showLoader(show) {
        this.userSeeking = show;
        this.els.loader?.classList.toggle('is-visible', show);
        if (!show) this.setMessage('');
    }

    #freezeFrame() {
        const source = this.#stageMedia();
        if (!source) return;
        const width = source.videoWidth || source.width;
        const height = source.videoHeight || source.height;
        if (!width || !height) return;
        try {
            const canvas = document.createElement('canvas');
            canvas.width = width;
            canvas.height = height;
            canvas.getContext('2d').drawImage(source, 0, 0);
            this.els.video.poster = canvas.toDataURL('image/jpeg', 0.72);
        } catch (_e) { /* cross-origin frame */ }
    }

    #stageMedia() {
        return this.container?.querySelector('.video-stage canvas') || this.els?.video || null;
    }

    #beginSeekLoad(action) {
        const token = ++this.seekToken;
        this.#showLoader(true);
        requestAnimationFrame(() => {
            if (token !== this.seekToken) return;
            requestAnimationFrame(() => {
                if (token !== this.seekToken) return;
                action();
            });
        });
    }

    #finishSeek() {
        this.targetUtc = null;
        this._advancing = false;
        this.#showLoader(false);
        if (this.els.video.poster) this.els.video.removeAttribute('poster');
    }

    /** Hide loader once media is ready. Keep it only while a seek to targetUtc is in flight. */
    #maybeFinishSeekOnReady() {
        if (this.els.video?.seeking) return;
        this.#finishSeek();
    }

    #startMoqClock() {
        this.#stopMoqClock();
        this._moqClock = setInterval(() => this.#updateProgramTime(), 250);
    }

    #stopMoqClock() {
        if (!this._moqClock) return;
        clearInterval(this._moqClock);
        this._moqClock = null;
    }

    async #ensureHls() {
        if (typeof Hls !== 'undefined') return;
        const local = new URL('../vendor/hls.js', import.meta.url).href;
        try {
            await loadScript(local);
        } catch (_e) {
            await loadScript('https://cdn.jsdelivr.net/npm/hls.js@1.5.13/dist/hls.min.js');
        }
    }

    #seekRanges() {
        if (this.options.fixedRange) return this._playlistRanges;
        return growOpenRange(
            this.recordingRanges,
            Math.floor(Date.now() / 1000),
            this._recordingOngoing,
        );
    }

    #markRecordingOngoing() {
        const last = this.recordingRanges[this.recordingRanges.length - 1];
        if (!last) {
            this._recordingOngoing = false;
            return;
        }
        const end = (Number(last.from) || 0) + (Number(last.duration) || 0);
        const fetchedAt = Math.floor((this._rangesFetchedAt || Date.now()) / 1000);
        this._recordingOngoing = end >= fetchedAt - RECORDING_ONGOING_SLACK;
    }

    #applyRecordingRanges() {
        this.timeline.setRanges(this.recordingRanges, { growLast: this._recordingOngoing });
    }

    async #refreshRanges({ force = false } = {}) {
        if (!this.options.name || !this.options.streamer_http) return;
        if (this._rangesFetch) return this._rangesFetch;
        if (!force && this._rangesFetchedAt && Date.now() - this._rangesFetchedAt < RANGES_TTL_MS) return;
        const run = this.#loadRecordingRanges();
        this._rangesFetch = run;
        try {
            await run;
        } finally {
            if (this._rangesFetch === run) this._rangesFetch = null;
        }
    }

    async #loadRecordingRanges() {
        try {
            this.recordingRanges = await fetchRecordingRanges(this.options);
            this._rangesFetchedAt = Date.now();
            this.#markRecordingOngoing();
            this.datepicker.setAvailableDays(buildAvailableDays(this.recordingRanges));
            if (!this.options.fixedRange) this.#applyRecordingRanges();
        } catch (e) {
            debugLog(this.options.debug, 'recording_status.json', e);
        }
    }

    #availableLiveProtos() {
        const list = [];
        if (this.options.streamer_http) list.push('hls');
        if (this.options.streamer_webrtc) list.push('webrtc');
        // MoQ UI hidden for now; still usable via explicit proto=moq.
        return list;
    }

    #useWebrtc() {
        return this.options.proto === 'webrtc' && Boolean(this.options.streamer_webrtc);
    }

    #useMoq() {
        return this.isLive && this.options.proto === 'moq' && Boolean(this.options.streamer_moq);
    }

    #setLiveProto(proto) {
        const next = proto === 'moq' || proto === 'webrtc' || proto === 'hls' ? proto : 'hls';
        if (next === 'moq' && !this.options.streamer_moq) return;
        if (next === 'webrtc' && !this.options.streamer_webrtc) return;
        if (next === 'hls' && !this.options.streamer_http) return;
        if (this.options.proto === next && this.isLive) return;
        this.options.proto = next;
        this.#syncProtoUi();
        if (this._liveStarted) this.goLive();
    }

    #fallbackWebrtcToHls(reason) {
        if (!this.isLive || this.options.proto !== 'webrtc' || !this.options.streamer_http) return false;
        debugLog(this.options.debug, 'webrtc fallback → hls', reason);
        this.#protoLog(`fallback webrtc → hls (${reason || 'failed'})`);
        this.options.proto = 'hls';
        this.#syncProtoUi();
        this.setMessage(t(this.options.locale, 'webrtcFallback'));
        this.#showLoader(true);
        queueMicrotask(() => {
            if (!this.isLive || this.options.proto !== 'hls') return;
            this.#loadLive();
        });
        return true;
    }

    #syncProtoBtn(key, label, active, available) {
        const btn = this.els?.[key];
        if (!btn) return;
        btn.hidden = !available;
        btn.textContent = label;
        btn.title = label;
        btn.classList.toggle('is-active', active);
    }

    #syncProtoUi() {
        const box = this.els?.['proto-switch'];
        if (!box) return;
        const available = this.#availableLiveProtos();
        box.hidden = available.length < 2 || Boolean(this.options.fixedRange);
        this.#syncProtoBtn(
            'proto-moq',
            protoLabel(this.options.locale, 'protoMoq', 'MoQ'),
            this.options.proto === 'moq',
            available.includes('moq'),
        );
        this.#syncProtoBtn(
            'proto-webrtc',
            protoLabel(this.options.locale, 'protoWebrtc', 'WebRTC'),
            this.options.proto === 'webrtc',
            available.includes('webrtc'),
        );
        this.#syncProtoBtn(
            'proto-hls',
            protoLabel(this.options.locale, 'protoHls', 'HLS'),
            this.options.proto === 'hls',
            available.includes('hls'),
        );
        this.container.classList.toggle('is-moq', this.#useMoq());
        this.#syncProtoLogUi();
    }

    #loadLive() {
        const moq = this.#useMoq();
        const webrtc = this.#useWebrtc();
        debugLog(this.options.debug, 'load live', moq ? 'moq' : webrtc ? 'webrtc' : 'hls');
        this.#freezeFrame();
        this.programStartTime = null;
        if (moq) {
            this.#stopMoqClock();
            this.engine?.destroy();
            this.webrtc?.destroy();
            this.#logMoqEnv();
            this.moq.load({
                pageUrl: moqPageUrl(this.options),
                query: authQuery(this.options),
                token: this.options.token,
                user: this.options.user,
                pass: this.options.pass,
                muted: this.els.video.muted,
            });
            return;
        }
        this.#stopMoqClock();
        this.moq?.destroy();
        if (webrtc) {
            this.engine?.destroy();
            this.#logWebrtcEnv();
            this.webrtc.load(liveWebrtcUrl(this.options), {
                token: this.options.token,
                user: this.options.user,
                pass: this.options.pass,
            });
            return;
        }
        this.webrtc?.destroy();
        this.engine.load(liveUrl(this.options), this.#hlsLoadOpts({ isLive: true, startTime: 0 }));
    }

    #load(url, startTime = 0) {
        debugLog(this.options.debug, 'load', url, { isLive: this.isLive, startTime });
        this.#freezeFrame();
        this.programStartTime = null;
        this.#stopMoqClock();
        this.moq?.destroy();
        this.webrtc?.destroy();
        this.engine.load(url, this.#hlsLoadOpts({ isLive: this.isLive, startTime }));
    }

    #hlsLoadOpts(extra) {
        return {
            retryPause: this.options.retryPause,
            headers: authHeaders(this.options),
            user: this.options.user,
            pass: this.options.pass,
            ...extra,
        };
    }

    #onManifest(info) {
        this.programStartTime = info.programStartTime;
        if (!this.programStartTime && this.archiveFrom != null) {
            this.programStartTime = new Date(this.archiveFrom * 1000);
        }

        if (info.duration && info.duration > 0 && !info.live) {
            this.isLive = false;
            if (this.pendingStartTime > 0 && this.programStartTime) {
                const start = Math.floor(this.programStartTime.getTime() / 1000);
                const wanted = this.targetUtc || (this.archiveFrom + this.pendingStartTime);
                this.els.video.currentTime = Math.max(0, wanted - start);
            }
        } else if (!this.archiveFrom) {
            this.isLive = true;
        }

        this.#setModeUi();
        if (!info.live && (this.options.fixedRange || this.episodeTo != null)) {
            this.#applyPlaylistWindow(info);
        } else if (!info.live) {
            const origin = this.programStartTime
                ? Math.floor(this.programStartTime.getTime() / 1000)
                : this.archiveFrom;
            this._playlistRanges = rangesFromFragments(info.fragments, origin);
            if (this.recordingRanges.length) this.#applyRecordingRanges();
            else this.timeline.setRanges(this._playlistRanges);
        } else {
            this.#applyRecordingRanges();
        }
        this.#snapStartToPlaylist(info);
        if (this.targetUtc != null) this.#pinPlayhead(this.targetUtc);
        else this.#updateProgramTime();

        if (this.#useMoq()) {
            this.#startMoqClock();
            this.#finishSeek();
            if (this.options.autoPlay) {
                this.els.play.innerHTML = ICON_PAUSE;
                this.els.play.title = t(this.options.locale, 'pause');
            } else {
                this.moq.pause();
            }
            return;
        }
        this.#stopMoqClock();

        if (this.options.autoPlay) {
            this.els.video.play().catch(() => {
                this.#finishSeek();
                this.setMessage(t(this.options.locale, 'clickPlay'));
            });
        } else {
            this.#finishSeek();
        }
    }

    #applyPlaylistWindow(info) {
        const startUnix = this.programStartTime
            ? Math.floor(this.programStartTime.getTime() / 1000)
            : this.archiveFrom;
        const ranges = rangesFromFragments(info.fragments, startUnix);
        this._playlistRanges = ranges.length
            ? ranges
            : (info.duration && startUnix != null
                ? [{ from: startUnix, duration: info.duration }]
                : []);
        if (this._playlistRanges.length) {
            const first = this._playlistRanges[0];
            const last = this._playlistRanges[this._playlistRanges.length - 1];
            const from = first.from;
            const to = last.from + last.duration;
            if (this.options.fixedRange) {
                this.episodeFrom = from;
                this.episodeTo = this.options.to != null ? Math.min(this.options.to, to) : to;
                this.archiveFrom = from;
                this.archiveDuration = Math.max(1, this.episodeTo - from);
            }
            if (this.episodeFrom != null && this.episodeTo != null) {
                this.timeline.lockRange(this.episodeFrom, this.episodeTo);
            }
            this.timeline.setRanges(this._playlistRanges);
        } else {
            if (this.options.fixedRange) this.timeline.setRanges([]);
            else this.#applyRecordingRanges();
            this.#finishSeek();
        }
    }

    #snapStartToPlaylist(info) {
        if (this.isLive || info?.live) return;
        const first = this._playlistRanges[0];
        if (!first) return;
        const wanted = this.targetUtc ?? (this.archiveFrom + (this.pendingStartTime || 0));
        if (wanted >= first.from - 1) return;

        if (this.archiveFrom != null && first.from > this.archiveFrom + 1) {
            if (this._playlistStartSnapped === first.from) return;
            this._playlistStartSnapped = first.from;
            const duration = this.episodeTo != null
                ? Math.max(1, this.episodeTo - first.from)
                : this.archiveDuration;
            this.openArchive(first.from, duration, 0, { resetView: false });
            return;
        }

        const firstFrag = (info.fragments || []).find((frag) => (Number(frag.duration) || 0) > 0);
        const mediaStart = Number.isFinite(firstFrag?.start) ? firstFrag.start : 0;
        this.pendingStartTime = Math.max(0, first.from - (this.archiveFrom ?? first.from));
        this.#pinPlayhead(first.from);
        try {
            this.els.video.currentTime = Math.max(0, mediaStart);
        } catch (_e) { /* ignore */ }
    }

    #playheadUtc() {
        if (this.isLive || !this.programStartTime) {
            return Math.floor(Date.now() / 1000);
        }
        return Math.floor(this.programStartTime.getTime() / 1000 + this.els.video.currentTime);
    }

    #setModeUi() {
        this.els['live-badge'].classList.toggle('not', !this.isLive);
        this.els['live-badge'].innerHTML = this.isLive
            ? t(this.options.locale, 'live')
            : `${ICON_BACK}${t(this.options.locale, 'live')}`;
        this.#syncProtoUi();
    }

    #updateProgramTime() {
        if (this.targetUtc != null || this.timeline.isSeeking) {
            const utc = this.targetUtc != null ? this.targetUtc : this.timeline.playheadUtc;
            this.els.timestamp.textContent = formatDateTime(new Date(utc * 1000));
            this.timeline.setPlayhead(utc);
            return;
        }
        const utc = this.#playheadUtc();
        if (this.#enforcePlaybackLimits(utc)) return;
        this.els.timestamp.textContent = formatDateTime(new Date(utc * 1000));
        if (this.isLive) {
            this.timeline.followLive(utc);
        } else {
            this.timeline.setPlayhead(utc);
            this.#maybeAdvanceClip(utc);
        }
    }

    #setRate(rate) {
        this.playbackRate = rate;
        this.els.video.playbackRate = rate;
        this.els.speed.textContent = `x${rate}`;
    }

    #syncMuteIcon() {
        const muted = this.els.video.muted || this.els.video.volume === 0;
        this.els['volume-icon'].innerHTML = muted ? ICON_VOLUME_OFF : ICON_VOLUME;
        this.els['volume-pos'].style.width = `${(muted ? 0 : this.els.video.volume) * 100}%`;
    }

    #setupBackButton() {
        const link = this.els?.back;
        const url = this.options.return_url;
        if (!link || !url) return;
        link.href = url;
        link.target = '_top';
        link.rel = 'noopener noreferrer';
        link.hidden = false;
        this.container.classList.add('has-back');
    }

    #bindUi() {
        const on = (el, event, handler) => {
            if (!el) return;
            el.addEventListener(event, handler);
            this.unsubscribers.push(() => el.removeEventListener(event, handler));
        };

        on(this.els.play, 'click', () => {
            if (this.#useMoq()) {
                if (this.moq.paused) this.play();
                else this.moq.pause();
                return;
            }
            if (this.els.video.paused) this.play().catch((e) => this.setMessage(e.message, true));
            else this.els.video.pause();
        });
        on(this.els.center, 'click', () => this.timeline.centerOn(this.#playheadUtc()));
        on(this.els['zoom-in'], 'click', () => this.timeline.zoom(-1, this.#playheadUtc()));
        on(this.els['zoom-out'], 'click', () => this.timeline.zoom(1, this.#playheadUtc()));
        on(this.els['pan-left'], 'click', () => this.timeline.pan(-1));
        on(this.els['pan-right'], 'click', () => this.timeline.pan(1));
        on(this.els.slower, 'click', () => {
            const idx = PLAYBACK_RATES.indexOf(this.playbackRate);
            if (idx > 0) this.#setRate(PLAYBACK_RATES[idx - 1]);
        });
        on(this.els.faster, 'click', () => {
            const idx = PLAYBACK_RATES.indexOf(this.playbackRate);
            if (idx < PLAYBACK_RATES.length - 1) this.#setRate(PLAYBACK_RATES[idx + 1]);
        });
        on(this.els['live-badge'], 'click', () => {
            if (this.options.fixedRange) return;
            if (!this.isLive) this.goLive();
        });
        on(this.els['proto-moq'], 'click', () => this.#setLiveProto('moq'));
        on(this.els['proto-webrtc'], 'click', () => this.#setLiveProto('webrtc'));
        on(this.els['proto-hls'], 'click', () => this.#setLiveProto('hls'));
        on(this.els.timestamp, 'click', (e) => {
            e.stopPropagation();
            this.#openDatePicker();
        });
        on(this.els['calendar-btn'], 'click', (e) => {
            e.stopPropagation();
            this.#openDatePicker();
        });
        on(this.els.snapshot, 'click', () => this.#screenshot());
        on(this.els.fullscreen, 'click', () => this.#fullscreen());
        on(this.container.querySelector('.PlayerInstance'), 'dblclick', (e) => {
            if (e.target.closest('.dvrp-footer, .dvr-timeline, .export-panel, .calendar, .dvrp-events, .dvrp-events-btn, .dvrp-button, .dvrp-volume-control, .timeStamp, .dvrp-back, .dvrp-proto, .itsLive')) return;
            e.preventDefault();
            if (this._videoZoom > 1) {
                this.#resetVideoZoom();
                return;
            }
            this.#fullscreen();
        });
        on(this.els.mute, 'click', () => {
            this.els.video.muted = !this.els.video.muted;
            this.#syncMuteIcon();
            if (this.#useMoq()) {
                if (this.els.video.muted) this.moq.mute();
                else this.moq.unmute();
            }
        });
        on(this.els['volume-slider'], 'click', (e) => {
            const rect = this.els['volume-slider'].getBoundingClientRect();
            const volume = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
            this.els.video.volume = volume;
            this.els.video.muted = volume === 0;
            this.volume = volume;
            this.#syncMuteIcon();
            if (this.#useMoq()) {
                if (this.els.video.muted) this.moq.mute();
                else this.moq.unmute();
            }
        });

        on(this.els['thumb-video'], 'loadeddata', () => {
            this.els['thumb-loader'].classList.remove('is-visible');
        });
        on(this.els['thumb-video'], 'error', () => {
            const video = this.els['thumb-video'];
            if (video.getAttribute('src') !== this._thumbSrc) return;
            if (this._thumbKind === 'unix') {
                this._thumbKind = 'date';
                this._thumbSrc = '';
                if (this._thumbUtc != null) this.#loadThumb(this._thumbUtc);
                return;
            }
            this.els['thumb-loader'].classList.remove('is-visible');
        });

        const onWheel = (e) => {
            const controls = this.els.footer.querySelector(':scope > .dvrp-controls');
            if (controls?.contains(e.target) && controls.scrollWidth > controls.clientWidth + 1 && Math.abs(e.deltaX) > Math.abs(e.deltaY)) {
                controls.scrollLeft += e.deltaX;
            }
            e.preventDefault();
        };
        this.els.footer.addEventListener('wheel', onWheel, { passive: false });
        this.unsubscribers.push(() => this.els.footer.removeEventListener('wheel', onWheel));

        const onDocClick = (e) => {
            if (!this.datepicker.isOpen()) return;
            if (this.els.datepicker.contains(e.target)) return;
            if (this.els['calendar-btn'].contains(e.target) || this.els.timestamp.contains(e.target)) return;
            this.datepicker.hide();
        };
        document.addEventListener('click', onDocClick);
        this.unsubscribers.push(() => document.removeEventListener('click', onDocClick));

        on(this.els.video, 'timeupdate', () => this.#updateProgramTime());
        on(this.els.video, 'seeked', () => {
            this._lastMediaTime = this.els.video.currentTime;
            this.#finishSeek();
        });
        on(this.els.video, 'seeking', () => {
            if (this.userSeeking || this.targetUtc != null) this.#showLoader(true);
        });
        on(this.els.video, 'waiting', () => {
            if (this.userSeeking || this.targetUtc != null) this.#showLoader(true);
        });
        on(this.els.video, 'playing', () => {
            this.#maybeFinishSeekOnReady();
            this.els.play.innerHTML = ICON_PAUSE;
            this.els.play.title = t(this.options.locale, 'pause');
            if (!this.isLive) this.#setRate(this.playbackRate);
        });
        on(this.els.video, 'canplay', () => {
            this.#maybeFinishSeekOnReady();
        });
        on(this.els.video, 'play', () => {
            this.els.play.innerHTML = ICON_PAUSE;
            this.els.play.title = t(this.options.locale, 'pause');
            if (this.isLive && (this.#useWebrtc() || this.#useMoq())) return;
            this.#setRate(this.playbackRate);
            if (!this.isLive) return;
            const sync = this.engine?.liveSyncPosition;
            try {
                if (typeof sync === 'number') this.els.video.currentTime = sync;
            } catch (_e) { /* ignore */ }
        });
        on(this.els.video, 'pause', () => {
            this.els.play.innerHTML = ICON_PLAY;
            this.els.play.title = t(this.options.locale, 'play');
        });
        on(this.els.video, 'volumechange', () => this.#syncMuteIcon());
        on(this.els.video, 'error', () => {
            this._advancing = false;
            this.#finishSeek();
            if (this.isLive) {
                this.setMessage(t(this.options.locale, 'networkRetry'));
                return;
            }
            this.setMessage(t(this.options.locale, 'videoError'), true);
        });
        on(this.els.video, 'ended', () => {
            this.#finishSeek();
            if (this.isLive) return;
            if (this.options.fixedRange || this.episodeTo != null) {
                this.#stopAtLimit(this.#playheadUtc());
                return;
            }
            this.#maybeAdvanceClip(this.#playheadUtc() + 1);
        });
    }

    #bindControlsVisibility() {
        const delay = this.options.hidecontrols;
        if (delay == null) return;
        if (delay === 0) {
            this.#setControlsVisible(false);
            return;
        }

        this._controlsHover = false;
        const overChrome = (target) => !!target?.closest?.('.dvrp-top, .dvrp-footer, .calendar, .dvrp-events');

        const bump = (e) => {
            if (this._compactControls) {
                this.#setControlsVisible(false);
                return;
            }
            this.#setControlsVisible(true);
            clearTimeout(this._hideControlsTimer);
            this._hideControlsTimer = null;
            if (this.datepicker?.isOpen() || this.exportActive || this._eventsOpen) return;
            if (this._controlsHover || (e && overChrome(e.target))) return;
            this._hideControlsTimer = setTimeout(() => {
                if (this._compactControls || this._controlsHover || this.datepicker?.isOpen() || this.exportActive || this._eventsOpen) return;
                this.#setControlsVisible(false);
            }, delay * 1000);
        };

        const onChromeEnter = () => {
            this._controlsHover = true;
            bump();
        };
        const onChromeLeave = () => {
            this._controlsHover = false;
            bump();
        };

        this.container.addEventListener('mousemove', bump);
        this.container.addEventListener('pointerdown', bump);
        const chrome = [this.els.top, this.els.footer, this.els.datepicker, this.els['events-panel']].filter(Boolean);
        for (const el of chrome) {
            el.addEventListener('pointerenter', onChromeEnter);
            el.addEventListener('pointerleave', onChromeLeave);
        }
        this._bumpControls = bump;
        this.unsubscribers.push(() => {
            clearTimeout(this._hideControlsTimer);
            this._bumpControls = null;
            this._controlsHover = false;
            this.container.removeEventListener('mousemove', bump);
            this.container.removeEventListener('pointerdown', bump);
            for (const el of chrome) {
                el.removeEventListener('pointerenter', onChromeEnter);
                el.removeEventListener('pointerleave', onChromeLeave);
            }
        });
        if (!this._compactControls) bump();
    }

    #controlsMin() {
        const min = Number(this.options.controls_min);
        return Number.isFinite(min) && min > 0 ? min : 0;
    }

    #bindCompactControls() {
        const min = this.#controlsMin();
        if (!min) return;

        const apply = (initial = false) => {
            const rect = this.container.getBoundingClientRect();
            const unknown = rect.width < 2 || rect.height < 2;
            const compact = unknown || Math.min(rect.width, rect.height) < min;
            if (!initial && compact === this._compactControls) return;
            this._compactControls = compact;
            this.container.classList.toggle('is-compact', compact);
            if (compact) {
                clearTimeout(this._hideControlsTimer);
                this.#setControlsVisible(false);
                return;
            }
            if (initial) {
                this.#setControlsVisible(this.options.hidecontrols !== 0);
                return;
            }
            if (this.options.hidecontrols === 0) this.#setControlsVisible(false);
            else if (this._bumpControls) this._bumpControls();
            else this.#setControlsVisible(true);
        };

        apply(true);

        if (typeof ResizeObserver === 'undefined') {
            const onResize = () => apply(false);
            window.addEventListener('resize', onResize);
            this.unsubscribers.push(() => window.removeEventListener('resize', onResize));
            return;
        }
        const observer = new ResizeObserver(() => apply(false));
        observer.observe(this.container);
        this.unsubscribers.push(() => observer.disconnect());
    }

    #setControlsVisible(show) {
        if (this._compactControls) show = false;
        this.container.classList.toggle('controls-hidden', !show);
        if (!show) this.#hideThumb();
    }

    #bindExport() {
        const on = (el, event, handler) => {
            if (!el) return;
            el.addEventListener(event, handler);
            this.unsubscribers.push(() => el.removeEventListener(event, handler));
        };

        if (!this.options.export) {
            if (this.els['export-btn']) this.els['export-btn'].style.display = 'none';
            if (this.els['export-panel']) this.els['export-panel'].style.display = 'none';
            return;
        }

        on(this.els['export-btn'], 'click', () => {
            if (this.exportActive) this.#closeExport();
            else this.#openExport();
        });
        on(this.els['export-download'], 'click', () => this.#downloadExport());
        on(this.els['export-cancel'], 'click', () => this.#closeExport());
        on(this.els['export-left'], 'pointerdown', (e) => this.#dragExport(e, 'left'));
        on(this.els['export-right'], 'pointerdown', (e) => this.#dragExport(e, 'right'));
        on(this.els['export-fill'], 'pointerdown', (e) => this.#dragExport(e, 'move'));
    }

    #openExport() {
        this.#refreshRanges({ force: true });
        const utc = this.#playheadUtc();
        const { from: minUtc, to: maxUtc } = this.#exportWindow();
        const span = Math.max(1, maxUtc - minUtc);
        const limit = this.options.export_limit || 600;
        const dur = Math.min(60, limit, span);
        this.exportActive = true;
        this.#resetExportDownload();
        this.els['export-btn']?.classList.add('active');
        this.els['export-panel']?.classList.add('is-open');
        this.#setControlsVisible(true);
        clearTimeout(this._hideControlsTimer);
        let start = utc - Math.floor(dur / 2);
        let end = start + dur;
        if (start < minUtc) {
            start = minUtc;
            end = start + dur;
        }
        if (end > maxUtc) {
            end = maxUtc;
            start = Math.max(minUtc, end - dur);
        }
        this.#setExportRange(start, end, 'right');
    }

    #closeExport() {
        this.#resetExportDownload();
        this.exportActive = false;
        this.exportFrom = null;
        this.exportTo = null;
        this.els['export-btn']?.classList.remove('active');
        this.els['export-panel']?.classList.remove('is-open');
        this.timeline.setExportRange(null, null);
        if (this.options.hidecontrols === 0) this.#setControlsVisible(false);
        else this._bumpControls?.();
    }

    #exportWindow() {
        const now = Math.floor(Date.now() / 1000);
        if (this.timeline.lockFrom != null && this.timeline.lockTo != null && this.timeline.lockTo > this.timeline.lockFrom) {
            return { from: this.timeline.lockFrom, to: Math.min(now, this.timeline.lockTo) };
        }
        if (this.episodeFrom != null && this.episodeTo != null && this.episodeTo > this.episodeFrom) {
            return { from: this.episodeFrom, to: Math.min(now, this.episodeTo) };
        }
        const viewEnd = this.timeline.viewFrom + this.timeline.viewDuration;
        return { from: this.timeline.viewFrom, to: Math.min(now, viewEnd) };
    }

    #setExportRange(from, to, moving = 'right') {
        const now = Math.floor(Date.now() / 1000);
        const { from: minUtc, to: maxUtc } = this.#exportWindow();
        const span = Math.max(1, maxUtc - minUtc);
        const limit = Math.min(this.options.export_limit || 600, span);
        let start = this.exportFrom ?? Math.min(from, to);
        let end = this.exportTo ?? Math.max(from, to);

        if (moving === 'move') {
            start = Math.floor(Math.min(from, to));
            end = Math.floor(Math.max(from, to));
        } else if (moving === 'left') {
            start = Math.floor(from);
            if (start > end - 1) start = end - 1;
            if (end - start > limit) end = start + limit;
        } else {
            end = Math.floor(to);
            if (end < start + 1) end = start + 1;
            if (end - start > limit) start = end - limit;
        }

        let duration = Math.max(1, Math.min(limit, end - start));
        if (end > maxUtc) {
            end = maxUtc;
            start = end - duration;
        }
        if (start < minUtc) {
            start = minUtc;
            end = start + duration;
        }
        if (end > maxUtc) end = maxUtc;
        if (end > now) {
            end = now;
            start = end - duration;
        }
        if (start < minUtc) start = minUtc;
        if (end <= start) end = Math.min(maxUtc, start + 1);

        this.exportFrom = Math.floor(start);
        this.exportTo = Math.floor(end);
        this.timeline.setExportRange(this.exportFrom, this.exportTo);
        if (this.els['export-from']) {
            this.els['export-from'].textContent = formatDateTime(new Date(this.exportFrom * 1000));
            this.els['export-to'].textContent = formatDateTime(new Date(this.exportTo * 1000));
            this.els['export-dur'].textContent = `(${formatExportDuration(this.exportTo - this.exportFrom)})`;
        }
    }

    #dragExport(e, side) {
        if (!this.exportActive) return;
        e.preventDefault();
        e.stopPropagation();
        this.#hideThumb();
        const handle = e.currentTarget;
        const pointerId = e.pointerId;
        const originUtc = this.timeline.utcAtClientX(e.clientX, { clamp: false });
        const originFrom = this.exportFrom;
        const originTo = this.exportTo;
        const originDur = Math.max(1, originTo - originFrom);
        const grabOffset = originUtc - originFrom;
        try { handle.setPointerCapture(pointerId); } catch (_err) { /* ignore */ }
        this.timeline.isPanning = true;

        const move = (ev) => {
            if (ev.pointerId !== pointerId) return;
            const utc = this.timeline.utcAtClientX(ev.clientX, { clamp: false });
            if (side === 'move') {
                this.#setExportRange(utc - grabOffset, utc - grabOffset + originDur, 'move');
                this.timeline.keepUtcVisible(utc);
                return;
            }
            if (side === 'left') this.#setExportRange(utc, this.exportTo, 'left');
            else this.#setExportRange(this.exportFrom, utc, 'right');
            this.timeline.keepUtcVisible(side === 'left' ? this.exportFrom : this.exportTo);
        };
        const up = (ev) => {
            if (ev.pointerId !== pointerId) return;
            handle.removeEventListener('pointermove', move);
            handle.removeEventListener('pointerup', up);
            handle.removeEventListener('pointercancel', up);
            try { handle.releasePointerCapture(pointerId); } catch (_err) { /* ignore */ }
            this.timeline.isPanning = false;
            this.timeline.holdFollow(1500);
        };
        handle.addEventListener('pointermove', move);
        handle.addEventListener('pointerup', up);
        handle.addEventListener('pointercancel', up);
    }

    async #downloadExport() {
        if (!this.exportActive || this._exportDownloading) return;
        if (this.exportFrom == null || this.exportTo == null) return;
        const duration = this.exportTo - this.exportFrom;
        if (duration < 1) return;
        this.#setExportDownloadBusy(true);
        const url = archiveMp4Url(this.options, this.exportFrom, duration);
        const filename = `archive-${this.exportFrom}-${duration}.mp4`;
        const authed = hasBasicAuth(this.options);
        try {
            if (authed) {
                const res = await authFetch(url, this.options);
                if (!res.ok) {
                    this.#resetExportDownload();
                    return;
                }
                const blobUrl = URL.createObjectURL(await res.blob());
                const link = document.createElement('a');
                link.href = blobUrl;
                link.download = filename;
                link.click();
                setTimeout(() => URL.revokeObjectURL(blobUrl), 2000);
            } else {
                const link = document.createElement('a');
                link.href = url;
                link.target = '_blank';
                link.rel = 'noopener';
                link.download = filename;
                link.click();
            }
        } catch (_e) {
            this.#resetExportDownload();
            return;
        }
        this._exportDownloadTimer = setTimeout(() => {
            if (this.exportActive) this.#resetExportDownload();
        }, authed ? 2000 : 8000);
    }

    #setExportDownloadBusy(busy) {
        this._exportDownloading = busy;
        const btn = this.els['export-download'];
        if (!btn) return;
        btn.disabled = busy;
        btn.textContent = t(this.options.locale, busy ? 'exportDownloading' : 'exportDownload');
    }

    #resetExportDownload() {
        if (this._exportDownloadTimer) {
            clearTimeout(this._exportDownloadTimer);
            this._exportDownloadTimer = null;
        }
        this.#setExportDownloadBusy(false);
    }

    #bindEvents() {
        if (!this.options.event) return;
        const on = (el, event, handler) => {
            if (!el) return;
            el.addEventListener(event, handler);
            this.unsubscribers.push(() => el.removeEventListener(event, handler));
        };
        on(this.els['events-btn'], 'click', (e) => {
            e.stopPropagation();
            if (this._eventsOpen) this.#closeEvents();
            else this.#openEvents();
        });
        on(this.els['events-close'], 'click', (e) => {
            e.stopPropagation();
            this.#closeEvents();
        });
        on(this.els['events-body'], 'scroll', () => this.#onEventsScroll());
        on(this.els['events-body'], 'wheel', (e) => this.#onEventsWheel(e));
    }

    #startEventsWatch() {
        if (!this.options.event) return;
        const tick = () => {
            const { from, to } = this.#eventsWindow();
            const key = `${from}:${to}`;
            if (key !== this._eventsWatchKey) {
                this._eventsWatchKey = key;
                this.#scheduleEvents();
            }
            this._eventsWatch = setTimeout(tick, 500);
        };
        tick();
    }

    #scheduleEvents() {
        if (!this.options.event) return;
        clearTimeout(this._eventsTimer);
        this._eventsTimer = setTimeout(() => this.#loadEvents(), 300);
    }

    #eventsAnchorUtc() {
        if (this.targetUtc != null) return this.targetUtc;
        if (this.timeline?.playheadUtc != null) return this.timeline.playheadUtc;
        return this.#playheadUtc();
    }

    #eventsWindow() {
        const utc = this.#eventsAnchorUtc();
        const from = startOfLocalDayTs(new Date(utc * 1000));
        const to = Math.max(from + 1, nextLocalMidnightTs(utc));
        return { from, to };
    }

    #eventsWindowFresh(from, to) {
        return this._eventsLoadedFrom === from && this._eventsLoadedTo === to;
    }

    #bindEventsLayout() {
        if (!this.options.event || !this.container) return;
        const apply = () => {
            const width = this.container.getBoundingClientRect().width;
            this.container.classList.toggle('events-stack', width > 0 && width < 880);
        };
        apply();
        if (typeof ResizeObserver === 'undefined') {
            const onResize = () => apply();
            window.addEventListener('resize', onResize);
            this.unsubscribers.push(() => window.removeEventListener('resize', onResize));
            return;
        }
        const observer = new ResizeObserver(() => apply());
        observer.observe(this.container);
        this.unsubscribers.push(() => observer.disconnect());
    }

    #openEvents() {
        this.datepicker?.hide();
        this._eventsOpen = true;
        this.container?.classList.add('events-open');
        if (this.els['events-panel']) this.els['events-panel'].hidden = false;
        this.els['events-btn']?.classList.add('is-open');
        this.els['events-btn']?.setAttribute('aria-expanded', 'true');
        this.#setControlsVisible(true);
        clearTimeout(this._hideControlsTimer);
        this._eventsLoadedFrom = null;
        this._eventsLoadedTo = null;
        this.#renderEvents('loading');
        this.#loadEvents();
    }

    #closeEvents() {
        const wasOpen = this._eventsOpen;
        this._eventsOpen = false;
        this.container?.classList.remove('events-open');
        if (this.els?.['events-panel']) this.els['events-panel'].hidden = true;
        this.els?.['events-btn']?.classList.remove('is-open');
        this.els?.['events-btn']?.setAttribute('aria-expanded', 'false');
        if (!wasOpen) return;
        if (this.options.hidecontrols === 0 || this._compactControls) this.#setControlsVisible(false);
        else if (this._bumpControls) this._bumpControls();
    }

    #syncEventsButton(visible) {
        const show = Boolean(this.options.event && visible);
        const btn = this.els?.['events-btn'];
        if (btn) btn.hidden = !show;
        this.container?.classList.toggle('has-events', show);
        if (!show) this.#closeEvents();
    }

    async #loadEvents() {
        if (!this.options.event || !this.options.name || !this.options.streamer_http || !this.timeline) return;
        const { from, to } = this.#eventsWindow();
        if (this.#eventsWindowFresh(from, to)) {
            if (this._eventsOpen) this.#renderEvents(this._eventsMode || 'list', { from, to, events: this._eventsList || [] });
            return;
        }
        const token = ++this._eventsToken;
        this._eventsPaging = null;
        try {
            const events = await fetchEvents(this.options, from, to, EVENTS_PAGE, 'desc');
            if (token !== this._eventsToken) return;
            this._eventsLoadedFrom = from;
            this._eventsLoadedTo = to;
            this._eventsList = events;
            this._eventsLeftTop = false;
            const newest = events[events.length - 1];
            const newestEnd = newest ? newest.from + Math.max(1, Number(newest.duration) || 1) : from;
            this._eventsHasOlder = events.length >= EVENTS_PAGE;
            this._eventsHasNewer = Boolean(newest) && newestEnd < Math.floor(Date.now() / 1000);
            this._eventsOlderFloor = null;
            this._eventsNewerFloor = null;
            this._eventsEmptyOlder = 0;
            this._eventsEmptyNewer = 0;
            this._eventsMode = events.length ? 'list' : 'empty';
            this.#syncEventsButton(events.length > 0);
            if (this._eventsOpen) this.#renderEvents(events.length ? 'list' : 'empty', { from, to, events, anchor: 'top' });
        } catch (e) {
            if (token !== this._eventsToken) return;
            this._eventsLoadedFrom = null;
            this._eventsLoadedTo = null;
            this._eventsMode = 'error';
            debugLog(this.options.debug, 'events.json', e);
            this.#syncEventsButton(true);
            if (this._eventsOpen) this.#renderEvents('error', { from, to });
        }
    }

    #onEventsScroll() {
        this.#scheduleEventPreviews();
        if (this._eventsScrollLock || !this.options.event || !this._eventsOpen || this._eventsPaging) return;
        if (this._eventsMode !== 'list') return;
        const body = this.els?.['events-body'];
        if (!body) return;
        const edge = 64;
        const atTop = body.scrollTop <= edge;
        if (!atTop) this._eventsLeftTop = true;
        if (body.scrollHeight - body.clientHeight <= 8) return;
        if (body.scrollTop + body.clientHeight >= body.scrollHeight - edge) {
            this.#loadOlderEvents();
            return;
        }
        if (atTop && this._eventsLeftTop) this.#loadNewerEvents();
    }

    #onEventsWheel(e) {
        if (this._eventsScrollLock || !this.options.event || !this._eventsOpen || this._eventsPaging) return;
        if (this._eventsMode !== 'list') return;
        const body = this.els?.['events-body'];
        if (!body || !e.deltaY) return;
        const edge = 64;
        if (e.deltaY < 0 && body.scrollTop <= edge) this.#loadNewerEvents();
        else if (e.deltaY > 0 && body.scrollTop + body.clientHeight >= body.scrollHeight - edge) this.#loadOlderEvents();
    }

    #olderEventsWindow() {
        const oldest = this._eventsList[0]?.from;
        if (oldest == null) return null;
        let lt = oldest;
        if (this._eventsOlderFloor != null) lt = Math.min(lt, this._eventsOlderFloor);
        const dayStart = startOfLocalDayTs(new Date(Math.max(0, lt - 1) * 1000));
        let gte = dayStart;
        if (gte >= lt) {
            gte = startOfLocalDayTs(new Date((dayStart - 1) * 1000));
            lt = dayStart;
        }
        if (gte >= lt) return null;
        return { gte, lt };
    }

    #newerEventsWindow() {
        const newest = this._eventsList[this._eventsList.length - 1];
        if (!newest) return null;
        let gte = newest.from + 1;
        if (this._eventsNewerFloor != null) gte = Math.max(gte, this._eventsNewerFloor);
        const now = Math.floor(Date.now() / 1000) + 1;
        if (gte >= now) return null;
        return { gte, lt: now };
    }

    async #loadOlderEvents() {
        if (this._eventsPaging || !this._eventsHasOlder) return false;
        const range = this.#olderEventsWindow();
        if (!range) {
            this._eventsHasOlder = false;
            return false;
        }
        this._eventsPaging = 'older';
        const token = this._eventsToken;
        try {
            const page = await fetchEvents(this.options, range.gte, range.lt, EVENTS_PAGE, 'desc');
            if (token !== this._eventsToken) return false;
            const added = this.#mergeEvents(page);
            if (added === 0) {
                this._eventsOlderFloor = range.gte;
                this._eventsEmptyOlder += 1;
                if (this._eventsEmptyOlder >= 3) this._eventsHasOlder = false;
            } else if (page.length < EVENTS_PAGE) {
                this._eventsOlderFloor = range.gte;
                this._eventsEmptyOlder = 0;
            } else {
                this._eventsEmptyOlder = 0;
            }
            this.#renderEvents(this._eventsMode || 'list', {
                from: this._eventsLoadedFrom,
                to: this._eventsLoadedTo,
                events: this._eventsList,
                anchor: 'keep',
            });
            return added > 0;
        } catch (e) {
            debugLog(this.options.debug, 'events.json', e);
            return false;
        } finally {
            if (this._eventsPaging === 'older') this._eventsPaging = null;
        }
    }

    async #loadNewerEvents() {
        if (this._eventsPaging || !this._eventsHasNewer) return false;
        const range = this.#newerEventsWindow();
        if (!range) {
            this._eventsHasNewer = false;
            return false;
        }
        this._eventsPaging = 'newer';
        const token = this._eventsToken;
        try {
            const page = await fetchEvents(this.options, range.gte, range.lt, EVENTS_PAGE, 'asc');
            if (token !== this._eventsToken) return false;
            const added = this.#mergeEvents(page);
            const now = Math.floor(Date.now() / 1000);
            if (added === 0) {
                this._eventsNewerFloor = range.lt;
                this._eventsEmptyNewer += 1;
                if (this._eventsEmptyNewer >= 3 || range.lt >= now) this._eventsHasNewer = false;
            } else if (page.length < EVENTS_PAGE) {
                this._eventsNewerFloor = range.lt;
                this._eventsEmptyNewer = 0;
                if (range.lt >= now) this._eventsHasNewer = false;
            } else {
                this._eventsEmptyNewer = 0;
                this._eventsHasNewer = true;
            }
            this.#renderEvents(this._eventsMode || 'list', {
                from: this._eventsLoadedFrom,
                to: this._eventsLoadedTo,
                events: this._eventsList,
                anchor: 'newer',
            });
            return added > 0;
        } catch (e) {
            debugLog(this.options.debug, 'events.json', e);
            return false;
        } finally {
            if (this._eventsPaging === 'newer') this._eventsPaging = null;
        }
    }

    #mergeEvents(page) {
        const seen = new Set((this._eventsList || []).map(eventKey));
        let added = 0;
        for (const ev of page || []) {
            const key = eventKey(ev);
            if (seen.has(key)) continue;
            seen.add(key);
            this._eventsList.push(ev);
            added += 1;
        }
        this._eventsList.sort((a, b) => a.from - b.from || a.topic.localeCompare(b.topic));
        if (added) this._eventsMode = 'list';
        return added;
    }

    #renderEvents(mode, { from, to, events = [], anchor = 'keep' } = {}) {
        const body = this.els?.['events-body'];
        const range = this.els?.['events-range'];
        if (!body) return;
        if (range) {
            range.textContent = from != null ? formatEventsDay(from) : '';
        }
        const scroll = body.scrollTop;
        const height = body.scrollHeight;
        this._eventsScrollLock = true;
        this.#dropEventPreviews();
        body.replaceChildren();
        if (mode === 'loading') {
            body.append(this.#eventsNote(t(this.options.locale, 'eventsLoading')));
            this.#unlockEventsScroll();
            return;
        }
        if (mode === 'error') {
            body.append(this.#eventsNote(t(this.options.locale, 'eventsError')));
            this.#unlockEventsScroll();
            return;
        }
        const ordered = [...events].sort((a, b) => b.from - a.from || a.topic.localeCompare(b.topic));
        for (const ev of ordered) {
            const row = document.createElement('button');
            row.type = 'button';
            row.className = 'dvrp-events__item';
            row.dataset.from = String(ev.from);
            if (ev.source) row.title = ev.source;
            const start = document.createElement('span');
            start.className = 'dvrp-events__start';
            start.textContent = formatDateTime(new Date(ev.from * 1000));
            const dur = document.createElement('span');
            dur.className = 'dvrp-events__dur';
            dur.textContent = formatEventDuration(ev.duration, this.options.locale);
            const meta = document.createElement('span');
            meta.className = 'dvrp-events__meta';
            meta.textContent = eventMeta(ev);
            if (this.#eventPreviewsEnabled()) {
                row.classList.add('has-preview');
                row.dataset.preview = String(this.#eventPreviewUtc(ev.from));
                row.append(this.#eventPreviewSlot(), start, dur, meta);
            } else {
                row.append(start, dur, meta);
            }
            row.addEventListener('click', () => {
                this.timeline.focusUtc(ev.from);
                this.seekToUtc(ev.from);
            });
            body.append(row);
        }
        if (anchor === 'top') this.#scrollEventsToCurrent(body);
        else if (anchor === 'newer') body.scrollTop = scroll + Math.max(0, body.scrollHeight - height);
        else body.scrollTop = scroll;
        this.#unlockEventsScroll(anchor);
    }

    #scrollEventsToCurrent(body) {
        const utc = this.#eventsAnchorUtc();
        const rows = body.querySelectorAll('.dvrp-events__item');
        let target = null;
        for (const row of rows) {
            if (Number(row.dataset.from) <= utc) {
                target = row;
                break;
            }
        }
        if (!target) target = rows[rows.length - 1] || null;
        body.scrollTop = target ? target.offsetTop : 0;
    }

    #unlockEventsScroll(anchor) {
        requestAnimationFrame(() => {
            if (anchor === 'top' && this.els?.['events-body']) this.#scrollEventsToCurrent(this.els['events-body']);
            this._eventsScrollLock = false;
            this.#watchEventPreviews();
        });
    }

    #eventPreviewsEnabled() {
        return Boolean(this.options.preview && this.options.dvr);
    }

    #eventPreviewUtc(from) {
        return Math.floor(Number(from) || 0);
    }

    #eventPreviewSlot() {
        const frame = document.createElement('span');
        frame.className = 'dvrp-events__preview';
        const video = document.createElement('video');
        video.muted = true;
        video.defaultMuted = true;
        video.playsInline = true;
        video.preload = 'none';
        video.tabIndex = -1;
        video.setAttribute('muted', '');
        video.setAttribute('playsinline', '');
        video.setAttribute('aria-hidden', 'true');
        video.addEventListener('loadeddata', () => {
            const box = video.parentElement;
            if (!box) return;
            box.classList.remove('is-loading');
            box.classList.add('is-ready');
            const pause = () => {
                try { video.pause(); } catch (_e) { /* ignore */ }
            };
            video.play().then(pause).catch(pause);
        });
        video.addEventListener('error', () => {
            const row = video.closest('.dvrp-events__item');
            if (!row || row.dataset.previewOn !== '1') return;
            const token = video.dataset.token;
            if (video.dataset.failed === token) return;
            video.dataset.failed = token;
            this.#retryEventPreview(row, video, Number(row.dataset.preview), video.dataset.kind || 'unix', token);
        });
        frame.append(video);
        return frame;
    }

    #watchEventPreviews() {
        const body = this.els?.['events-body'];
        if (!body || !this.#eventPreviewsEnabled()) return;
        const rows = body.querySelectorAll('.dvrp-events__item[data-preview]');
        if (!rows.length) return;
        if (typeof IntersectionObserver === 'undefined') {
            this.#scheduleEventPreviews();
            return;
        }
        if (!this._eventPreviewObserver) {
            this._eventPreviewObserver = new IntersectionObserver((entries) => {
                for (const entry of entries) {
                    if (entry.isIntersecting) this._eventPreviewVisible.add(entry.target);
                    else {
                        this._eventPreviewVisible.delete(entry.target);
                        this.#unmountEventPreview(entry.target);
                    }
                }
                this.#scheduleEventPreviews();
            }, { root: body, threshold: 0.6 });
        }
        for (const row of rows) this._eventPreviewObserver.observe(row);
    }

    #scheduleEventPreviews() {
        if (!this.#eventPreviewsEnabled()) return;
        clearTimeout(this._eventPreviewSettle);
        this._eventPreviewSettle = setTimeout(() => this.#loadVisibleEventPreviews(), 300);
    }

    #loadVisibleEventPreviews() {
        this._eventPreviewSettle = null;
        if (!this.#eventPreviewsEnabled() || !this._eventsOpen) return;
        if (!this._eventPreviewObserver) {
            this.#syncEventPreviews();
            return;
        }
        const body = this.els?.['events-body'];
        for (const row of this._eventPreviewVisible) {
            if (!row.isConnected || !body || !this.#eventPreviewRowVisible(row, body)) {
                this._eventPreviewVisible.delete(row);
                this.#unmountEventPreview(row);
                continue;
            }
            this.#mountEventPreview(row);
        }
    }

    #eventPreviewRowVisible(row, body) {
        const root = body.getBoundingClientRect();
        const rect = row.getBoundingClientRect();
        const visible = Math.min(rect.bottom, root.bottom) - Math.max(rect.top, root.top);
        return visible >= Math.min(rect.height, root.height) * 0.6;
    }

    #syncEventPreviews() {
        const body = this.els?.['events-body'];
        if (!body) return;
        for (const row of body.querySelectorAll('.dvrp-events__item[data-preview]')) {
            if (this.#eventPreviewRowVisible(row, body)) this.#mountEventPreview(row);
            else this.#unmountEventPreview(row);
        }
    }

    #mountEventPreview(row) {
        if (!row?.isConnected || row.dataset.previewOn === '1') return;
        const video = row.querySelector('.dvrp-events__preview video');
        if (!video) return;
        row.dataset.previewOn = '1';
        this.#attachEventPreview(row, video, Number(row.dataset.preview), row.dataset.previewKind || 'unix');
    }

    #attachEventPreview(row, video, utc, kind) {
        const frame = video.parentElement;
        const url = previewMp4Url(this.options, utc, kind);
        const token = String((Number(video.dataset.token) || 0) + 1);
        video.dataset.token = token;
        video.dataset.kind = kind;
        row.dataset.previewKind = kind;
        row.dataset.previewUrl = url;
        frame?.classList.remove('is-error', 'is-ready');
        frame?.classList.add('is-loading');
        const apply = (src) => {
            if (!row.isConnected || row.dataset.previewOn !== '1' || video.dataset.token !== token) return;
            if (video.dataset.srcApplied === src && video.getAttribute('src')) {
                frame?.classList.remove('is-loading');
                frame?.classList.add('is-ready');
                return;
            }
            video.dataset.srcApplied = src;
            video.preload = 'auto';
            video.src = src;
        };
        if (!hasBasicAuth(this.options)) {
            apply(url);
            return;
        }
        this.#requestEventPreviewBlob(url).then(apply).catch((err) => {
            if (err?.name === 'AbortError' || video.dataset.token !== token) return;
            this.#retryEventPreview(row, video, utc, kind, token);
        });
    }

    #retryEventPreview(row, video, utc, kind, token) {
        if (!row.isConnected || row.dataset.previewOn !== '1' || video.dataset.token !== token) return;
        if (hasBasicAuth(this.options)) this.#releaseEventPreviewJob(row.dataset.previewUrl);
        if (kind !== 'date') {
            this.#attachEventPreview(row, video, utc, 'date');
            return;
        }
        const frame = video.parentElement;
        frame?.classList.remove('is-loading');
        frame?.classList.add('is-error');
        video.removeAttribute('src');
        try { video.load(); } catch (_e) { /* ignore */ }
    }

    #unmountEventPreview(row) {
        if (!row || row.dataset.previewOn !== '1') return;
        row.dataset.previewOn = '0';
        const video = row.querySelector('.dvrp-events__preview video');
        if (video) {
            video.dataset.token = String((Number(video.dataset.token) || 0) + 1);
            video.removeAttribute('src');
            video.dataset.srcApplied = '';
            try { video.load(); } catch (_e) { /* ignore */ }
        }
        const frame = row.querySelector('.dvrp-events__preview');
        frame?.classList.remove('is-loading', 'is-ready');
        if (hasBasicAuth(this.options)) this.#releaseEventPreviewJob(row.dataset.previewUrl);
    }

    #dropEventPreviews() {
        clearTimeout(this._eventPreviewSettle);
        this._eventPreviewSettle = null;
        this._eventPreviewVisible?.clear();
        const body = this.els?.['events-body'];
        if (!body) return;
        for (const row of body.querySelectorAll('.dvrp-events__item[data-preview]')) {
            this._eventPreviewObserver?.unobserve(row);
            this.#unmountEventPreview(row);
        }
    }

    #requestEventPreviewBlob(url) {
        let job = this._eventPreviewJobs.get(url);
        if (!job) {
            job = { ctrl: new AbortController(), refs: 0, src: null, promise: null };
            this._eventPreviewJobs.set(url, job);
        }
        job.refs += 1;
        if (job.src) return Promise.resolve(job.src);
        if (!job.promise) {
            job.promise = authFetch(url, this.options, { signal: job.ctrl.signal })
                .then((res) => {
                    if (!res.ok) throw new Error(`HTTP ${res.status}`);
                    return res.blob();
                })
                .then((blob) => {
                    const src = URL.createObjectURL(blob);
                    job.src = src;
                    job.promise = null;
                    return src;
                })
                .catch((err) => {
                    job.promise = null;
                    throw err;
                });
        }
        return job.promise;
    }

    #releaseEventPreviewJob(url) {
        const job = this._eventPreviewJobs.get(url);
        if (!job) return;
        job.refs = Math.max(0, job.refs - 1);
        if (job.refs > 0) return;
        if (!job.src) {
            job.ctrl.abort();
            this._eventPreviewJobs.delete(url);
            return;
        }
        const idle = [];
        for (const [key, item] of this._eventPreviewJobs) {
            if (item.refs <= 0 && item.src) idle.push(key);
        }
        while (idle.length > 24) {
            const key = idle.shift();
            const stale = this._eventPreviewJobs.get(key);
            if (stale?.src) URL.revokeObjectURL(stale.src);
            this._eventPreviewJobs.delete(key);
        }
    }

    #disposeEventPreviews() {
        this.#dropEventPreviews();
        this._eventPreviewObserver?.disconnect();
        this._eventPreviewObserver = null;
        for (const job of this._eventPreviewJobs.values()) {
            job.ctrl.abort();
            if (job.src) URL.revokeObjectURL(job.src);
        }
        this._eventPreviewJobs.clear();
    }

    #eventsNote(text) {
        const note = document.createElement('div');
        note.className = 'dvrp-events__note';
        note.textContent = text;
        return note;
    }

    #openDatePicker() {
        if (!this.options.dvr || this.options.fixedRange) return;
        this.#closeEvents();
        const open = () => {
            const base = this.datepicker.selectedDayStart
                ? new Date(this.datepicker.selectedDayStart * 1000)
                : new Date(this.#playheadUtc() * 1000);
            this.datepicker.toggle(base);
        };
        this.#refreshRanges({ force: true }).then(open);
    }

    #screenshot() {
        const source = this.#stageMedia();
        const width = source?.videoWidth || source?.width;
        const height = source?.videoHeight || source?.height;
        if (!source || !width || !height) {
            this.setMessage(t(this.options.locale, 'screenshotFail'), true);
            return;
        }
        const canvas = document.createElement('canvas');
        canvas.width = width;
        canvas.height = height;
        canvas.getContext('2d').drawImage(source, 0, 0);
        try {
            const link = document.createElement('a');
            link.download = `screenshot-${new Date().toISOString().replace(/[:.]/g, '-')}.png`;
            link.href = canvas.toDataURL('image/png');
            link.click();
        } catch (_e) {
            this.setMessage(t(this.options.locale, 'screenshotFail'), true);
        }
    }

    #bindVideoZoom() {
        if (!this.options.panzoom) return;
        const stage = this.container.querySelector('.video-stage');
        const video = this.els.video;
        if (!stage || !video) return;

        const chrome = '.dvrp-footer, .dvrp-top, .calendar, .timeline__thumbnail, .export-panel, .dvrp-back';
        const min = 1;
        const max = 8;

        const apply = () => {
            const media = this.#stageMedia();
            if (!media) return;
            if (this._videoZoom <= 1.001) {
                this._videoZoom = 1;
                this._videoPanX = 0;
                this._videoPanY = 0;
                media.style.transform = '';
                this.els.video.style.transform = '';
                stage.classList.remove('is-zoomed', 'is-panning');
                return;
            }
            const rect = stage.getBoundingClientRect();
            const width = rect.width;
            const height = rect.height;
            this._videoPanX = Math.min(0, Math.max(width - width * this._videoZoom, this._videoPanX));
            this._videoPanY = Math.min(0, Math.max(height - height * this._videoZoom, this._videoPanY));
            media.style.transformOrigin = '0 0';
            media.style.transform = `translate(${this._videoPanX}px, ${this._videoPanY}px) scale(${this._videoZoom})`;
            stage.classList.add('is-zoomed');
        };

        const onWheel = (e) => {
            if (e.target.closest(chrome)) return;
            e.preventDefault();
            e.stopPropagation();
            const rect = stage.getBoundingClientRect();
            const px = e.clientX - rect.left;
            const py = e.clientY - rect.top;
            let factor = Math.exp(-e.deltaY * 0.003);
            factor = Math.min(1.25, Math.max(0.8, factor));
            const next = Math.max(min, Math.min(max, this._videoZoom * factor));
            if (next === this._videoZoom) return;
            const vx = (px - this._videoPanX) / this._videoZoom;
            const vy = (py - this._videoPanY) / this._videoZoom;
            this._videoZoom = next;
            this._videoPanX = px - vx * next;
            this._videoPanY = py - vy * next;
            apply();
        };

        let dragging = false;
        let lastX = 0;
        let lastY = 0;
        let pointerId = null;

        const onDown = (e) => {
            if (e.pointerType === 'mouse' && e.button !== 0) return;
            if (this._videoZoom <= 1) return;
            if (e.target.closest(chrome)) return;
            dragging = true;
            pointerId = e.pointerId;
            lastX = e.clientX;
            lastY = e.clientY;
            stage.classList.add('is-panning');
            try { stage.setPointerCapture(pointerId); } catch (_err) { /* ignore */ }
        };
        const onMove = (e) => {
            if (!dragging || e.pointerId !== pointerId) return;
            this._videoPanX += e.clientX - lastX;
            this._videoPanY += e.clientY - lastY;
            lastX = e.clientX;
            lastY = e.clientY;
            apply();
        };
        const onUp = (e) => {
            if (!dragging || (pointerId != null && e.pointerId !== pointerId)) return;
            dragging = false;
            pointerId = null;
            stage.classList.remove('is-panning');
        };

        stage.addEventListener('wheel', onWheel, { passive: false });
        stage.addEventListener('pointerdown', onDown);
        stage.addEventListener('pointermove', onMove);
        stage.addEventListener('pointerup', onUp);
        stage.addEventListener('pointercancel', onUp);
        this.unsubscribers.push(() => {
            stage.removeEventListener('wheel', onWheel);
            stage.removeEventListener('pointerdown', onDown);
            stage.removeEventListener('pointermove', onMove);
            stage.removeEventListener('pointerup', onUp);
            stage.removeEventListener('pointercancel', onUp);
            this.#resetVideoZoom();
        });
    }

    #resetVideoZoom() {
        this._videoZoom = 1;
        this._videoPanX = 0;
        this._videoPanY = 0;
        const media = this.#stageMedia();
        const stage = this.container.querySelector('.video-stage');
        if (media) media.style.transform = '';
        if (this.els?.video) this.els.video.style.transform = '';
        stage?.classList.remove('is-zoomed', 'is-panning');
    }

    #fullscreen() {
        const fsEl = () => document.fullscreenElement || document.webkitFullscreenElement;
        const inIframe = window.parent !== window;
        const notifyParent = (action) => {
            if (!inIframe) return;
            window.parent.postMessage({ type: 'mtx-dvr-fullscreen', action }, '*');
        };

        if (fsEl()) {
            document.exitFullscreen?.() || document.webkitExitFullscreen?.();
            notifyParent('exit');
            return;
        }

        const node = this.container;
        let req;
        try {
            if (node.requestFullscreen) req = node.requestFullscreen();
            else if (node.webkitRequestFullscreen) node.webkitRequestFullscreen();
        } catch {
            notifyParent('enter');
            return;
        }

        // Mosaic still listens for mtx-dvr-fullscreen. Notify only if this
        // document did not enter fullscreen (no allow=fullscreen, old browser).
        // Do not notify on success: mosaic would treat the iframe as already
        // fullscreen and immediately exit.
        if (!inIframe) return;
        const fallback = () => { if (!fsEl()) notifyParent('enter'); };
        if (req && typeof req.then === 'function') {
            Promise.resolve(req).then(fallback).catch(() => notifyParent('enter'));
        } else {
            requestAnimationFrame(fallback);
        }
    }
}

function protoLabel(locale, key, fallback) {
    const value = t(locale, key);
    return value && value !== key ? value : fallback;
}

function normalizeLiveOptions(options) {
    const opts = options || {};
    const query = {};
    try {
        new URLSearchParams(String(opts.query || '').replace(/^\?/, '')).forEach((value, key) => {
            query[key] = value;
        });
    } catch (_e) { /* ignore */ }

    opts.streamer_webrtc = String(opts.streamer_webrtc || query.streamer_webrtc || '').replace(/\/+$/, '');
    opts.streamer_http = String(opts.streamer_http || query.streamer_http || '').replace(/\/+$/, '');
    opts.streamer_moq = String(opts.streamer_moq || query.streamer_moq || '').replace(/\/+$/, '');
    opts.proto = resolveLiveProto({
        proto: query.proto || opts.proto || 'webrtc',
        streamer_http: opts.streamer_http,
        streamer_webrtc: opts.streamer_webrtc,
        streamer_moq: opts.streamer_moq,
    });
    return opts;
}

function liveWebrtcUrl(options) {
    const path = withQuery(joinUrl(options.streamer_webrtc, options.name, 'whep'), authQuery(options));
    try {
        return new URL(path, window.location.href).toString();
    } catch (_e) {
        return path;
    }
}

function rangesFromFragments(fragments, fallbackFrom) {
    const items = [];
    for (const frag of fragments || []) {
        const duration = Number(frag.duration) || 0;
        if (duration <= 0) continue;
        let start = null;
        if (frag.programDateTime) start = Math.floor(new Date(frag.programDateTime).getTime() / 1000);
        else if (fallbackFrom != null && Number.isFinite(frag.start)) start = Math.floor(fallbackFrom + frag.start);
        if (start == null) continue;
        const prev = items[items.length - 1];
        if (prev && Math.abs(prev.from + prev.duration - start) <= 2) {
            prev.duration = start + duration - prev.from;
        } else {
            items.push({ from: start, duration });
        }
    }
    return items;
}

function formatEventsDay(from) {
    const date = new Date(from * 1000);
    return `${pad(date.getDate())}.${pad(date.getMonth() + 1)}.${date.getFullYear()}`;
}

function formatEventDuration(seconds, locale) {
    const total = Math.max(0, Math.round(Number(seconds) || 0));
    if (total < 60) return locale === 'en' ? `${total} s` : `${total} с`;
    const hrs = Math.floor(total / 3600);
    const mins = Math.floor((total % 3600) / 60);
    const secs = total % 60;
    if (hrs) return `${hrs}:${pad(mins)}:${pad(secs)}`;
    return `${mins}:${pad(secs)}`;
}

function eventKey(ev) {
    return `${ev.from}|${ev.topic}|${ev.source}`;
}

function eventMeta(ev) {
    const parts = String(ev.topic || '').split('/').filter(Boolean);
    const topic = parts.slice(-2).join(' / ');
    const kind = String(ev.class || '').trim();
    if (kind && topic) return `${kind} · ${topic}`;
    return kind || topic;
}

function formatExportDuration(seconds) {
    const total = Math.max(0, Math.floor(seconds));
    const hrs = Math.floor(total / 3600);
    const mins = Math.floor((total % 3600) / 60);
    const secs = total % 60;
    if (hrs) return `${hrs}:${pad(mins)}:${pad(secs)}`;
    return `${mins}:${pad(secs)}`;
}
