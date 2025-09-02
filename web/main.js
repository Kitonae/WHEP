(() => {
  const qs = (s) => document.querySelector(s);
  const logEl = qs('#log');
  const videoEl = qs('#video');
  const endpointEl = qs('#endpoint');
  const iceEl = qs('#ice');
  const muteEl = qs('#mute');
  const autoplayEl = qs('#autoplay');
  const playBtn = qs('#play');
  const stopBtn = qs('#stop');

  let pc = null;
  let resource = null;

  const log = (msg) => {
    const t = new Date().toISOString();
    logEl.textContent += `[${t}] ${msg}\n`;
    logEl.scrollTop = logEl.scrollHeight;
  };

  const parseIceServers = (text) => {
    if (!text.trim()) return undefined;
    const parts = text.split(',').map(s => s.trim()).filter(Boolean);
    return parts.length ? [{ urls: parts }] : undefined;
  };

  const waitForIceGathering = (pc) => new Promise((resolve) => {
    if (pc.iceGatheringState === 'complete') return resolve();
    const check = () => {
      if (pc.iceGatheringState === 'complete') {
        pc.removeEventListener('icegatheringstatechange', check);
        resolve();
      }
    };
    pc.addEventListener('icegatheringstatechange', check);
  });

  const play = async () => {
    try {
      const endpoint = endpointEl.value || '/whep';
      const config = { iceServers: parseIceServers(iceEl.value) };
      pc = new RTCPeerConnection(config);
      videoEl.muted = !!muteEl.checked;
      videoEl.autoplay = !!autoplayEl.checked;
      const inbound = new MediaStream();
      pc.ontrack = (ev) => {
        inbound.addTrack(ev.track);
        videoEl.srcObject = inbound;
      };
      pc.onconnectionstatechange = () => log(`state=${pc.connectionState}`);
      pc.oniceconnectionstatechange = () => log(`ice=${pc.iceConnectionState}`);

      const offer = await pc.createOffer({ offerToReceiveVideo: true, offerToReceiveAudio: false });
      await pc.setLocalDescription(offer);
      await waitForIceGathering(pc); // non-trickle: include candidates in offer

      log('POST offer → ' + endpoint);
      const resp = await fetch(endpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/sdp' },
        body: pc.localDescription.sdp
      });
      if (!resp.ok) throw new Error(`WHEP POST failed: ${resp.status}`);
      resource = resp.headers.get('Location');
      const answerSdp = await resp.text();
      await pc.setRemoteDescription({ type: 'answer', sdp: answerSdp });
      log('Answer set. Streaming...');
      playBtn.disabled = true; stopBtn.disabled = false;
    } catch (err) {
      log('Error: ' + (err && err.message || err));
      await stop();
    }
  };

  const stop = async () => {
    try {
      if (resource) {
        try { await fetch(resource, { method: 'DELETE' }); } catch {}
      }
    } finally {
      resource = null;
      if (pc) { pc.close(); pc = null; }
      playBtn.disabled = false; stopBtn.disabled = true;
    }
  };

  playBtn.addEventListener('click', play);
  stopBtn.addEventListener('click', stop);
})();

