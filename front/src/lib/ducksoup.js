document.addEventListener("DOMContentLoaded", () => {
    console.log("[DuckSoup] v1.1.1")
});

// Config

const DEFAULT_CONSTRAINTS = {
    video: {
        width: { ideal: 800 },
        height: { ideal: 600 },
        frameRate: { ideal: 30 },
        facingMode: { ideal: "user" },
    },
    audio: {
        sampleSize: 16,
        channelCount: 1,
        autoGainControl: false,
        latency: { ideal: 0.003 },
        echoCancellation: false,
        noiseSuppression: false,
    },
};

const DEFAULT_RTC_CONFIG = {
    iceServers: [
        {
            urls: "stun:stun.l.google.com:19302",
        },
    ],
};

const IS_SAFARI = (() => {
    const ua = navigator.userAgent;
    const containsChrome = ua.indexOf("Chrome") > -1;
    const containsSafari = ua.indexOf("Safari") > -1;
    return containsSafari && !containsChrome;
})();

// Pure functions

const areOptionsValid = ({ roomId, userId, duration }) => {
    return typeof roomId !== 'undefined' &&
        typeof userId !== 'undefined' &&
        !isNaN(duration);
}

const clean = (obj) => {
    for (let prop in obj) {
        if (obj[prop] === null || obj[prop] === undefined) delete obj[prop];
    }
    return obj;
}

const parseJoinPayload = (peerOptions) => {
    // explicit list, without origin
    let { roomId, userId, duration, size, width, height, audioFx, videoFx, frameRate, namespace, videoCodec } = peerOptions;
    if (!["VP8", "H264"].includes(videoCodec)) videoCodec = null;
    if (isNaN(size)) size = null;
    if (isNaN(width)) width = null;
    if (isNaN(height)) height = null;
    if (isNaN(frameRate)) frameRate = null;

    return clean({ roomId, userId, duration, size, width, height, audioFx, videoFx, frameRate, namespace, videoCodec });
}

const forceMozillaMono = (sdp) => {
    if (!window.navigator.userAgent.includes("Mozilla")) return sdp;
    return sdp
        .split("\r\n")
        .map((line) => {
            if (line.startsWith("a=fmtp:111")) {
                return line.replace("stereo=1", "stereo=0");
            } else {
                return line;
            }
        })
        .join("\r\n");
};

const processSDP = (sdp) => {
    const output = forceMozillaMono(sdp);
    return output;
};

const kbps = (bytes, duration) => {
    const result = (8 * bytes) / duration / 1024;
    return result.toFixed(1);
};

const looseJSONParse = (str) => {
    try {
        return JSON.parse(str);
    } catch (error) {
        console.error(error);
    }
}

// DuckSoup

class DuckSoup {

    // API

    constructor(mountEl, peerOptions, embedOptions) {
        if (!areOptionsValid(peerOptions)) {
            this._postStop({ kind: "error", payload: "Invalid DuckSoup options" });
        } else {
            // replace mountEl contents
            while (mountEl.firstChild) {
                mountEl.removeChild(mountEl.firstChild);
            }
            this._mountEl = mountEl;
            this._signalingUrl = peerOptions.signalingUrl;
            this._rtcConfig = peerOptions.rtcConfig || DEFAULT_RTC_CONFIG;
            this._joinPayload = parseJoinPayload(peerOptions);
            this._constraints = {
                audio: { ...DEFAULT_CONSTRAINTS.audio, ...peerOptions.audio },
                video: { ...DEFAULT_CONSTRAINTS.video, ...peerOptions.video },
            };
            this._debug = embedOptions && embedOptions.debug;
            this._callback = embedOptions && embedOptions.callback;
            if (this._debug) {
                this._debugInfo = {
                    now: Date.now(),
                    audioBytesSent: 0,
                    audioBytesReceived: 0,
                    videoBytesSent: 0,
                    videoBytesReceived: 0
                };
            }
        }
    };

    audioControl(effectName, property, value, transitionDuration) {
        this._control("audio", effectName, property, value, transitionDuration);
    }

    videoControl(effectName, property, value, transitionDuration) {
        this._control("video", effectName, property, value, transitionDuration);
    }

    stop(wsStatusCode) {
        this._stopRTC()
        // see status codes https://developer.mozilla.org/en-US/docs/Web/API/CloseEvent#status_codes
        this._ws.close(wsStatusCode);
    }

    get stream() {
        return this._stream;
    }

    // Inner methods

    async _initialize() {
        try {
            // async calls
            await this._startRTC();
            this._running = true;
        } catch (err) {
            this._postStop({ kind: "error", payload: err });
        }
    }

    _checkControl(name, property, value, duration) {
        const durationValid = typeof duration === "undefined" || typeof duration === "number";
        return typeof name === "string" && typeof property === "string" && typeof value === "number" && durationValid;
    }

    _control(kind, name, property, value, duration) {
        if(!this._checkControl(name, property, value, duration)) return;
        this._ws.send(
            JSON.stringify({
                kind: "control",
                payload: JSON.stringify({ kind, name, property, value, ...(duration && { duration }) }),
            })
        );
    }


    _postMessage(message, force) {
        if (this._callback && (this._running || force)) this._callback(message);
    }

    _postStop(reason) {
        const message = typeof reason === "string" ? { kind: reason } : reason;
        this._postMessage(message);
        if (this._debugIntervalId) clearInterval(this._debugIntervalId);
    }

    _stopRTC() {
        if(this._stream) {
            this._stream.getTracks().forEach((track) => track.stop());
        }
        if(this._pc) {
            this._pc.close();
        }
    }

    async _startRTC() {
        // RTCPeerConnection
        const pc = new RTCPeerConnection(this._rtcConfig);
        this._pc = pc;

        // Add local tracks before signaling
        const stream = await navigator.mediaDevices.getUserMedia(this._constraints);
        stream.getTracks().forEach((track) => {
            pc.addTrack(track, stream);
        });
        this._stream = stream;

        // Signaling
        const ws = new WebSocket(this._signalingUrl);
        this._ws = ws;

        ws.onopen = () => {
            ws.send(
                JSON.stringify({
                    kind: "join",
                    payload: JSON.stringify(this._joinPayload),
                })
            );
        };

        ws.onclose = () => {
            this._postStop("disconnection");
            this._stopRTC();
        };

        ws.onerror = (event) => {
            this._postStop({ kind: "error", payload: event.data });
            this.stop(4000); // used as error
        };

        ws.onmessage = async (event) => {
            let message = looseJSONParse(event.data);

            console.log("message", message);

            if (message.kind === "offer") {
                const offer = looseJSONParse(message.payload);
                pc.setRemoteDescription(offer);
                const answer = await pc.createAnswer();
                answer.sdp = processSDP(answer.sdp);
                pc.setLocalDescription(answer);
                ws.send(
                    JSON.stringify({
                        kind: "answer",
                        payload: JSON.stringify(answer),
                    })
                );
            } else if (message.kind === "candidate") {
                const candidate = looseJSONParse(message.payload);
                try {
                    pc.addIceCandidate(candidate);
                } catch (error) {
                    console.error(error)
                }
            } else if (message.kind === "start") {
                this._postMessage({ kind: "start" }, true); // force with true since player is not already running
            } else if (message.kind === "ending") {
                this._postMessage({ kind: "ending" });
            } else if (message.kind.startsWith("error") || message.kind === "end") {
                this._postStop(message);
                this.stop(1000); // Normal Closure
            }
        };

        pc.onicecandidate = (e) => {
            if (!e.candidate) return;
            ws.send(
                JSON.stringify({
                    kind: "candidate",
                    payload: JSON.stringify(e.candidate),
                })
            );
        };

        pc.ontrack = (event) => {
            let el = document.createElement(event.track.kind);
            el.id = event.track.id;
            el.srcObject = event.streams[0];
            el.autoplay = true;
            if(event.track.kind === "video") {
                el.style.width = "100%";
                el.style.height = "100%";
            }
            this._mountEl.appendChild(el);

            event.streams[0].onremovetrack = ({ track }) => {
                const el = document.getElementById(track.id);
                if (el) el.parentNode.removeChild(el);
            };
        };

        // Stats
        if (this._debug) {
            this._debugIntervalId = setInterval(() => this._updateStats(), 1000);
        }
    }

    async _updateStats() {
        const pc = this._pc;
        const pcStats = await pc.getStats();
        const newNow = Date.now();
        let newAudioBytesSent = 0;
        let newAudioBytesReceived = 0;
        let newVideoBytesSent = 0;
        let newVideoBytesReceived = 0;

        pcStats.forEach((report) => {
            if (report.type === "outbound-rtp" && report.kind === "audio") {
                newAudioBytesSent += report.bytesSent;
            } else if (report.type === "inbound-rtp" && report.kind === "audio") {
                newAudioBytesReceived += report.bytesReceived;
            } else if (report.type === "outbound-rtp" && report.kind === "video") {
                newVideoBytesSent += report.bytesSent;
            } else if (report.type === "inbound-rtp" && report.kind === "video") {
                newVideoBytesReceived += report.bytesReceived;
            }
        });

        const elapsed = (newNow - this._debugInfo.now) / 1000;
        const audioUp = kbps(
            newAudioBytesSent - this._debugInfo.audioBytesSent,
            elapsed
        );
        const audioDown = kbps(
            newAudioBytesReceived - this._debugInfo.audioBytesReceived,
            elapsed
        );
        const videoUp = kbps(
            newVideoBytesSent - this._debugInfo.videoBytesSent,
            elapsed
        );
        const videoDown = kbps(
            newVideoBytesReceived - this._debugInfo.videoBytesReceived,
            elapsed
        );
        this._postMessage({
            kind: "stats",
            payload: { audioUp, audioDown, videoUp, videoDown }
        });
        this._debugInfo = {
            now: newNow,
            audioBytesSent: newAudioBytesSent,
            audioBytesReceived: newAudioBytesReceived,
            videoBytesSent: newVideoBytesSent,
            videoBytesReceived: newVideoBytesReceived
        };
    }
}

// API

window.DuckSoup = {
    render: async (mountEl, peerOptions, embedOptions) => {
        const player = new DuckSoup(mountEl, peerOptions, embedOptions);
        await player._initialize();
        return player;
    }
}