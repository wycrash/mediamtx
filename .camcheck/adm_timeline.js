import { formatDayLabel, formatHms, formatTimeLabel, floorToLocalStep, nextLocalMidnightTs, startOfLocalDayTs } from './utils.js';

export const MIN_ZOOM = 60;
export const MAX_ZOOM = 7 * 86400;

const TIME_STEPS = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 14400, 21600, 43200, 86400];

export function growOpenRange(ranges, now, enabled) {
    if (!enabled || !ranges?.length) return ranges || [];
    const last = ranges[ranges.length - 1];
    const from = Number(last.from) || 0;
    const duration = Math.max(0, Number(last.duration) || 0);
    if (now <= from + duration) return ranges;
    const grown = ranges.slice(0, -1);
    grown.push({ from, duration: now - from });
    return grown;
}

export class Timeline {
    #anim = null;
    #holdFollowUntil = 0;
    #wheelAccum = 0;
    #wheelDir = 0;
    #wheelCoolUntil = 0;

    constructor(els, { centeredSeek = false } = {}) {
        this.els = els;
        this.centeredSeek = Boolean(centeredSeek);
        this.viewFrom = Math.floor(Date.now() / 1000) - 3600;
        this.viewDuration = 3600;
        this.playheadUtc = Math.floor(Date.now() / 1000);
        this.ranges = [];
        this.growLastRange = false;
        this.exportFrom = null;
        this.exportTo = null;
        this.isSeeking = false;
        this.isPanning = false;
        this.onPreview = () => {};
        this.onCommit = () => {};
        this.onViewChange = () => {};
        this.onHover = () => {};
        this.onHoverEnd = () => {};
        this.lockFrom = null;
        this.lockTo = null;
    }

    bind({ onPreview, onCommit, onViewChange, onHover, onHoverEnd } = {}) {
        this.onPreview = onPreview || (() => {});
        this.onCommit = onCommit || (() => {});
        this.onViewChange = onViewChange || (() => {});
        this.onHover = onHover || (() => {});
        this.onHoverEnd = onHoverEnd || (() => {});
        const { seek } = this.els;

        seek.addEventListener('mousemove', (e) => {
            if (this.isPanning || this.isSeeking) return;
            this.onHover(this.utcAtClientX(e.clientX), e.clientX);
        });
        seek.addEventListener('mouseleave', () => {
            if (!this.isSeeking && !this.isPanning) this.onHoverEnd();
        });

        seek.addEventListener('pointerdown', (e) => this.#onPointerDown(e));
        seek.addEventListener('wheel', (e) => this.#onWheel(e), { passive: false });
    }

    #onPointerDown(e) {
        if (e.pointerType === 'mouse' && e.button !== 0) return;
        if (e.target?.closest?.('.export-range__handle, .export-range__fill')) return;
        e.preventDefault();
        this.#cancelAnim();

        const seek = this.els.seek;
        const pointerId = e.pointerId;
        const startX = e.clientX;
        const startFrom = this.viewFrom;
        const grabbingPlayhead = Boolean(e.target?.closest?.('.now-marker'));
        let mode = grabbingPlayhead ? 'seek' : 'pending';
        const threshold = e.pointerType === 'touch' ? 6 : 8;

        try { seek.setPointerCapture(pointerId); } catch (_err) { /* ignore */ }

        if (mode === 'seek') {
            this.#beginSeek(this.utcAtClientX(e.clientX, { clamp: false }), e.clientX);
        }

        const move = (ev) => {
            if (ev.pointerId !== pointerId) return;
            const dx = ev.clientX - startX;
            if (mode === 'pending' && Math.abs(dx) >= threshold) {
                mode = 'pan';
                this.isPanning = true;
                this.#holdFollowUntil = performance.now() + 2000;
                this.onHoverEnd();
                seek.classList.add('is-panning');
            }
            if (mode === 'pan') {
                ev.preventDefault();
                const width = seek.getBoundingClientRect().width || 1;
                this.setView(startFrom - (dx / width) * this.viewDuration, this.viewDuration);
                return;
            }
            if (mode === 'seek') {
                ev.preventDefault();
                this.#beginSeek(this.utcAtClientX(ev.clientX, { clamp: false }), ev.clientX);
                return;
            }
            this.onHover(this.utcAtClientX(ev.clientX), ev.clientX);
        };

        const up = (ev) => {
            if (ev.pointerId !== pointerId) return;
            seek.removeEventListener('pointermove', move);
            seek.removeEventListener('pointerup', up);
            seek.removeEventListener('pointercancel', up);
            try { seek.releasePointerCapture(pointerId); } catch (_err) { /* ignore */ }
            seek.classList.remove('is-panning', 'is-seeking');

            if (mode === 'pan') {
                this.isPanning = false;
                this.#holdFollowUntil = performance.now() + 1500;
                this.onViewChange();
                return;
            }

            const utc = this.utcAtClientX(ev.clientX, { clamp: mode !== 'seek' });
            this.#beginSeek(utc, ev.clientX);
            this.onPreview(utc);
            this.isSeeking = false;
            if (this.centeredSeek) this.#followPlayheadView();
            this.onCommit(utc);
            if (!seek.matches(':hover')) this.onHoverEnd();
        };

        seek.addEventListener('pointermove', move);
        seek.addEventListener('pointerup', up);
        seek.addEventListener('pointercancel', up);
    }

    #beginSeek(utc, clientX) {
        this.isSeeking = true;
        this.#holdFollowUntil = performance.now() + 2000;
        this.els.seek.classList.add('is-seeking');
        this.keepUtcVisible(utc);
        this.setPlayhead(utc);
        this.onHover(utc, clientX);
    }

    utcAtClientX(clientX, { clamp = true } = {}) {
        const rect = this.els.seek.getBoundingClientRect();
        let percent = rect.width ? (clientX - rect.left) / rect.width : 0;
        if (clamp) percent = Math.max(0, Math.min(1, percent));
        return Math.floor(this.viewFrom + percent * this.viewDuration);
    }

    secondsPerPixel() {
        const width = this.els.seek.getBoundingClientRect().width;
        return width ? this.viewDuration / width : 1;
    }

    keepUtcVisible(utc) {
        if (utc == null || !Number.isFinite(utc)) return;
        if (utc < this.viewFrom) {
            this.setView(utc, this.viewDuration);
        } else if (utc > this.viewFrom + this.viewDuration) {
            this.setView(utc - this.viewDuration, this.viewDuration);
        }
    }

    holdFollow(ms = 1500) {
        this.#holdFollowUntil = performance.now() + ms;
    }

    #onWheel(e) {
        e.preventDefault();
        e.stopPropagation();
        let dy = e.deltaY;
        if (e.deltaMode === 1) dy *= 16;
        else if (e.deltaMode === 2) dy *= 120;
        const dir = dy > 0 ? 1 : dy < 0 ? -1 : 0;
        if (!dir) return;
        if (dir !== this.#wheelDir) {
            this.#wheelAccum = 0;
            this.#wheelDir = dir;
        }
        this.#wheelAccum += dy;
        if (this.#anim || performance.now() < this.#wheelCoolUntil) return;
        if (Math.abs(this.#wheelAccum) < 48) return;
        this.#wheelAccum = 0;
        this.#wheelCoolUntil = performance.now() + 200;
        this.zoom(dir, this.playheadUtc);
    }

    setView(from, duration, { animate = false } = {}) {
        const fitted = this.#fitView(from, duration);
        if (!animate) {
            this.#cancelAnim();
            this.viewFrom = fitted.from;
            this.viewDuration = fitted.duration;
            this.render();
            return;
        }
        this.#animateView(fitted.from, fitted.duration);
    }

    lockRange(from, to) {
        this.lockFrom = from;
        this.lockTo = to;
        this.setView(from, Math.max(1, to - from));
    }

    clearLock() {
        this.lockFrom = null;
        this.lockTo = null;
    }

    #fitView(from, duration) {
        if (this.lockFrom != null && this.lockTo != null && this.lockTo > this.lockFrom) {
            const lockDur = this.lockTo - this.lockFrom;
            const minDur = Math.min(MIN_ZOOM, lockDur);
            const nextDuration = Math.max(minDur, Math.min(lockDur, duration));
            const nextFrom = Math.max(this.lockFrom, Math.min(this.lockTo - nextDuration, from));
            return { from: nextFrom, duration: nextDuration };
        }
        const nextDuration = Math.max(MIN_ZOOM, Math.min(MAX_ZOOM, duration));
        const now = Date.now() / 1000;
        const maxFuture = nextDuration * (this.centeredSeek ? 0.5 : 0.12);
        let nextFrom = from;
        if (nextFrom + nextDuration > now + maxFuture) {
            nextFrom = now + maxFuture - nextDuration;
        }
        return { from: nextFrom, duration: nextDuration };
    }

    setPlayhead(utc) {
        this.playheadUtc = utc;
        if (this.#canFollowPlayhead()) {
            this.#followPlayheadView();
            return;
        }
        this.#renderPlayhead();
        this.#renderRanges();
    }

    #canFollowPlayhead() {
        return this.centeredSeek
            && !this.isPanning
            && !this.isSeeking
            && !this.#anim
            && performance.now() >= this.#holdFollowUntil;
    }

    #followPlayheadView() {
        const fitted = this.#fitView(this.playheadUtc - this.viewDuration / 2, this.viewDuration);
        if (Math.abs(fitted.from - this.viewFrom) < 0.05 && Math.abs(fitted.duration - this.viewDuration) < 0.05) {
            this.#renderPlayhead();
            this.#renderRanges();
            return;
        }
        this.setView(fitted.from, fitted.duration);
    }

    setRanges(ranges, { growLast = false } = {}) {
        this.ranges = ranges || [];
        this.growLastRange = growLast;
        this.#renderRanges();
    }

    zoomBy(factor, centerUtc = this.playheadUtc) {
        if (!this.centeredSeek) this.#holdFollowUntil = performance.now() + 1500;
        const { min, max } = this.#zoomLimits();
        const newDuration = Math.max(min, Math.min(max, this.viewDuration * factor));
        const ratio = (centerUtc - this.viewFrom) / this.viewDuration;
        const newFrom = centerUtc - newDuration * Math.max(0, Math.min(1, ratio));
        this.setView(newFrom, newDuration, { animate: true });
        return { from: newFrom, duration: newDuration };
    }

    zoom(direction, centerUtc = this.playheadUtc) {
        return this.zoomBy(direction < 0 ? 1 / 1.4 : 1.4, centerUtc);
    }

    pan(direction) {
        this.#holdFollowUntil = performance.now() + 1500;
        const shift = this.viewDuration * 0.35 * direction;
        this.setView(this.viewFrom + shift, this.viewDuration, { animate: true });
        return { from: this.viewFrom + shift, duration: this.viewDuration };
    }

    centerOn(utc = this.playheadUtc) {
        this.setView(utc - this.viewDuration / 2, this.viewDuration, { animate: true });
    }

    /** Shift the scale so `utc` sits under the current playhead. */
    focusUtc(utc) {
        if (utc == null || !Number.isFinite(utc)) return;
        this.#holdFollowUntil = performance.now() + 2000;
        this.#cancelAnim();
        const fraction = this.#playheadFraction();
        this.setView(utc - this.viewDuration * fraction, this.viewDuration);
    }

    #playheadFraction() {
        const fraction = (this.playheadUtc - this.viewFrom) / this.viewDuration;
        if (!Number.isFinite(fraction) || fraction < 0 || fraction > 1) return 0.5;
        return fraction;
    }

    get isAnimating() {
        return this.#anim != null;
    }

    followLive(nowUtc) {
        if (this.#anim || this.isSeeking || this.isPanning) return;
        if (performance.now() < this.#holdFollowUntil) {
            this.setPlayhead(nowUtc);
            return;
        }
        if (this.centeredSeek) {
            this.setPlayhead(nowUtc);
            return;
        }
        const edge = this.viewFrom + this.viewDuration * 0.88;
        if (nowUtc > edge || nowUtc < this.viewFrom) {
            this.setView(nowUtc - this.viewDuration * 0.88, this.viewDuration);
        } else {
            this.setPlayhead(nowUtc);
        }
    }

    reset() {
        this.els.marks.innerHTML = '';
        this.els.ranges.innerHTML = '';
    }

    render() {
        this.#renderMarks();
        this.#renderRanges();
        this.#renderPlayhead();
        this.#renderExport();
    }

    #cancelAnim() {
        if (this.#anim) {
            cancelAnimationFrame(this.#anim);
            this.#anim = null;
        }
    }

    #animateView(toFrom, toDur) {
        this.#cancelAnim();
        const from0 = this.viewFrom;
        const dur0 = this.viewDuration;
        const t0 = performance.now();
        const ms = 180;
        const step = (now) => {
            const t = Math.min(1, (now - t0) / ms);
            const ease = 1 - (1 - t) ** 3;
            this.viewFrom = from0 + (toFrom - from0) * ease;
            this.viewDuration = dur0 + (toDur - dur0) * ease;
            this.render();
            if (t < 1) {
                this.#anim = requestAnimationFrame(step);
            } else {
                this.viewFrom = toFrom;
                this.viewDuration = toDur;
                this.#anim = null;
                this.render();
                this.onViewChange();
            }
        };
        this.#anim = requestAnimationFrame(step);
    }

    #percent(utc) {
        return ((utc - this.viewFrom) / this.viewDuration) * 100;
    }

    #renderPlayhead() {
        const pct = this.#percent(this.playheadUtc);
        this.els.thumb.style.left = `${Math.max(0, Math.min(100, pct))}%`;
        this.els.thumb.style.display = (pct < 0 || pct > 100) ? 'none' : 'block';
    }

    setExportRange(from, to) {
        this.exportFrom = from;
        this.exportTo = to;
        this.#renderExport();
    }

    #renderExport() {
        const el = this.els['export-sel'];
        if (!el) return;
        if (this.exportFrom == null || this.exportTo == null) {
            el.hidden = true;
            return;
        }
        const leftPct = this.#percent(this.exportFrom);
        const rightPct = this.#percent(this.exportTo);
        const left = Math.max(0, Math.min(100, Math.min(leftPct, rightPct)));
        const right = Math.max(0, Math.min(100, Math.max(leftPct, rightPct)));
        el.hidden = false;
        el.style.left = `${left}%`;
        el.style.width = `${Math.max(0.4, right - left)}%`;
    }

    #zoomLimits() {
        if (this.lockFrom != null && this.lockTo != null && this.lockTo > this.lockFrom) {
            const lockDur = this.lockTo - this.lockFrom;
            return { min: Math.min(MIN_ZOOM, lockDur), max: lockDur };
        }
        return { min: MIN_ZOOM, max: MAX_ZOOM };
    }

    #timeStep() {
        const target = this.viewDuration / 8;
        return TIME_STEPS.find((step) => step >= target) || 86400;
    }

    #renderMarks() {
        this.els.marks.innerHTML = '';
        const viewFrom = this.viewFrom;
        const viewEnd = this.viewFrom + this.viewDuration;
        const firstDay = startOfLocalDayTs(new Date(viewFrom * 1000));
        const lastDay = startOfLocalDayTs(new Date((viewEnd - 1) * 1000));
        const spansDays = lastDay > firstDay;

        const marks = [];
        const add = (utc, text, day, extraClass = '') => {
            if (utc < viewFrom - 1 || utc > viewEnd + 1) return;
            marks.push({ utc: Math.max(viewFrom, Math.min(viewEnd, utc)), text, day, extraClass });
        };

        if (spansDays) {
            add(viewFrom, formatDayLabel(new Date(viewFrom * 1000)), true, 'is-start');
            let midnight = nextLocalMidnightTs(viewFrom);
            while (midnight < viewEnd) {
                add(midnight, formatDayLabel(new Date(midnight * 1000)), true);
                midnight = nextLocalMidnightTs(midnight);
            }
        }

        const step = this.#timeStep();
        const label = (date) => (step < 60 ? formatHms(date) : formatTimeLabel(date));
        let tick = floorToLocalStep(viewFrom, step);
        if (tick < viewFrom) tick += step;
        while (tick <= viewEnd) {
            const date = new Date(tick * 1000);
            const isMidnight = date.getHours() === 0 && date.getMinutes() === 0;
            if (!isMidnight) {
                const tooCloseToDay = marks.some((mark) => (
                    mark.day && Math.abs(mark.utc - tick) / this.viewDuration < 0.045
                ));
                if (!tooCloseToDay) add(tick, label(date), false);
            }
            tick += step;
        }

        if (!spansDays && !marks.length) {
            add(viewFrom, label(new Date(viewFrom * 1000)), false, 'is-start');
        }

        for (const mark of marks) {
            const span = document.createElement('span');
            span.className = [mark.day ? 'day' : '', mark.extraClass].filter(Boolean).join(' ');
            span.style.left = `${this.#percent(mark.utc)}%`;
            span.innerHTML = `${mark.text}<i class="marker${mark.day ? ' day' : ''}"></i>`;
            this.els.marks.appendChild(span);
        }
    }

    #renderRanges() {
        this.els.ranges.innerHTML = '';
        const viewEnd = this.viewFrom + this.viewDuration;
        const now = Math.floor(Date.now() / 1000);
        const pastEnd = Math.min(now, viewEnd);

        if (pastEnd > this.viewFrom) {
            this.#addSegment('miss', this.viewFrom, pastEnd - this.viewFrom);
        }

        for (const range of growOpenRange(this.ranges, now, this.growLastRange)) {
            const from = Number(range.from) || 0;
            const duration = Number(range.duration) || 0;
            const left = Math.max(from, this.viewFrom);
            const right = Math.min(from + duration, viewEnd);
            if (right > left) this.#addSegment('hit', left, right - left);
        }
    }

    #addSegment(type, from, duration) {
        const el = document.createElement('div');
        el.className = type;
        el.style.left = `${this.#percent(from)}%`;
        el.style.width = `${(duration / this.viewDuration) * 100}%`;
        this.els.ranges.appendChild(el);
    }
}

export function snapToRange(utc, ranges) {
    if (!ranges?.length) return utc;
    for (const range of ranges) {
        const from = Number(range.from) || 0;
        const to = from + (Number(range.duration) || 0);
        if (utc >= from && utc <= to) return utc;
    }
    let nearest = ranges[0];
    let best = Infinity;
    for (const range of ranges) {
        const from = Number(range.from) || 0;
        const to = from + (Number(range.duration) || 0);
        const dist = utc < from ? from - utc : utc - to;
        if (dist < best) {
            best = dist;
            nearest = range;
        }
    }
    const from = Number(nearest.from) || utc;
    const to = from + (Number(nearest.duration) || 0);
    return utc < from ? from : to;
}
