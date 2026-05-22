// agentchute.dev — ASCII animation engine.
//
// Each animation is a sequence of FRAMES. A frame is a string showing one
// instant of the diagram. The engine cycles through the frames at a fixed
// interval (or steps via controls). Each frame is independent (no diff
// merging), which keeps the code small and the source readable.

const HANDOFF_FRAMES = [
  // Step 1 — alice composing
  `┌──────────────────┐                        ┌──────────────────┐
│ ALICE (Claude)   │                        │ BOB (codex)      │
│ inbox: 0         │                        │ inbox: 0         │
│ status: active   │                        │ status: active   │
└──────────────────┘                        └──────────────────┘
         │
         │ 1. alice writes a message addressed to bob...
         │                                              ░
         │                                              ░
         │                                              ░
         │                                              ░
                                                        │
                                                        │ bob still idle
                                                        │ (no notification yet)`,

  // Step 2 — message lands in bob's inbox
  `┌──────────────────┐                        ┌──────────────────┐
│ ALICE (Claude)   │                        │ BOB (codex)      │
│ inbox: 0         │                        │ inbox: 1  ← +1   │
│ status: active   │                        │ status: active   │
└──────────────────┘                        └──────────────────┘
         │                                            ▲
         │ 2. message lands in inbox/bob/             │
         │                                            │
         ●━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━●
         (file moves into bob's inbox via atomic rename)
                                                        │
                                                        │ bob's inbox
                                                        │ now has 1 unread`,

  // Step 3 — wake poke fires
  `┌──────────────────┐                        ┌──────────────────┐
│ ALICE (Claude)   │                        │ BOB (codex)      │
│ inbox: 0         │                        │ inbox: 1         │
│ status: active   │                        │ status: WAKING   │
└──────────────────┘                        └──────────────────┘
         │                                            ▲
         │                                            │
         │ 3. alice fires wake poke (tmux send-keys)  │
         │                                            │
         └ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ▶
                ░  ░  ░  ░  ░  ░  ░  ░  ░  ░
              (tmux: "[agentchute:tmux] check inbox" + Enter)`,

  // Step 4 — bob processes
  `┌──────────────────┐                        ┌──────────────────┐
│ ALICE (Claude)   │                        │ BOB (codex)      │
│ inbox: 0         │                        │ inbox: 0  ← read │
│ status: active   │                        │ status: active   │
└──────────────────┘                        └──────────────────┘
                                                      │
         4. bob runs 'check':                         │
            • read the message                        │
            • archive it                              │
            • update last_seen                        │
                                                      │
                                                      ▼
                                              [message archived
                                               at .vendor/loop/archive/]`,

  // Step 5 — reply
  `┌──────────────────┐                        ┌──────────────────┐
│ ALICE (Claude)   │                        │ BOB (codex)      │
│ inbox: 1  ← +1   │                        │ inbox: 0         │
│ status: WAKING   │                        │ status: active   │
└──────────────────┘                        └──────────────────┘
         ▲
         │ 5. bob replies (same flow, opposite direction)
         │       • bob writes to inbox/alice/
         │       • bob pokes alice's wake_target
         │       • alice processes
         ●◀━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━●
         ◀ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┘
                                                       wake poke

       symmetric: same protocol, no central broker.`,
];

const REVIEW_FRAMES = [
  // Step 1 — claude-code about to send
  `┌────────────────────────────────────────────────────────────┐
│ SHARED INBOX MEDIUM                                        │
│ ref CLI stores it at .<vendor>/loop/                       │
└────────────────────────────────────────────────────────────┘

  task traffic ━━━━           wake pokes ─ ─ ─ ─

┌────────────────┐      ┌────────────────┐      ┌────────────────┐
│ CLAUDE-CODE    │      │ CODEX          │      │ GEMINI-CLI     │
│ inbox: 0       │      │ inbox: 0       │      │ inbox: 0       │
└────────┬───────┘      └────────────────┘      └────────────────┘
         │
         │ 1. claude-code composes "request review" — fans out
         │    to codex AND gemini-cli (no broadcast; two sends)
         │`,

  // Step 2 — messages land in codex + gemini inboxes
  `┌────────────────────────────────────────────────────────────┐
│ SHARED INBOX MEDIUM                                        │
│ ref CLI stores it at .<vendor>/loop/                       │
└────────────────────────────────────────────────────────────┘

  task traffic ━━━━           wake pokes ─ ─ ─ ─

┌────────────────┐      ┌────────────────┐      ┌────────────────┐
│ CLAUDE-CODE    │      │ CODEX          │      │ GEMINI-CLI     │
│ inbox: 0       │      │ inbox: 1 ← +1  │      │ inbox: 1 ← +1  │
└────────┬───────┘      └────────▲───────┘      └────────▲───────┘
         │ 2. messages landed     │                       │
         ●━━━━━━━━━━━━━━━━━━━━━━━━●                       │
         ●━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━●
         │                       (review requests pending)`,

  // Step 3 — wake pokes
  `┌────────────────────────────────────────────────────────────┐
│ SHARED INBOX MEDIUM                                        │
│ ref CLI stores it at .<vendor>/loop/                       │
└────────────────────────────────────────────────────────────┘

  task traffic ━━━━           wake pokes ─ ─ ─ ─

┌────────────────┐      ┌────────────────┐      ┌────────────────┐
│ CLAUDE-CODE    │      │ CODEX          │      │ GEMINI-CLI     │
│ inbox: 0       │      │ status: WAKING │      │ status: WAKING │
└────────┬───────┘      └────────▲───────┘      └────────▲───────┘
         │                       │                       │
         │ 3. wake pokes fire    │                       │
         └─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─┘                       │
         └─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─┘
                        (tmux send-keys "[agentchute:tmux] check inbox" to each pane)`,

  // Step 4 — codex + gemini reply with findings
  `┌────────────────────────────────────────────────────────────┐
│ SHARED INBOX MEDIUM                                        │
│ ref CLI stores it at .<vendor>/loop/                       │
└────────────────────────────────────────────────────────────┘

  task traffic ━━━━           wake pokes ─ ─ ─ ─

┌────────────────┐      ┌────────────────┐      ┌────────────────┐
│ CLAUDE-CODE    │      │ CODEX          │      │ GEMINI-CLI     │
│ inbox: 2 ← +2  │      │ inbox: 0       │      │ inbox: 0       │
└────────▲───────┘      └────────┬───────┘      └────────┬───────┘
         │                       │                       │
         │ 4. both reply with findings                    │
         ●━━━━━━━━━━━━━━━━━━━━━━━┘                       │
         ●━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┘
         ◀ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ wake claude`,

  // Step 5 — claude-code consolidates + asks sign-off + watchdog idle
  `┌────────────────────────────────────────────────────────────┐
│ SHARED INBOX MEDIUM                                        │
│ ref CLI stores it at .<vendor>/loop/                       │
└────────────────────────────────────────────────────────────┘

  task traffic ━━━━           wake pokes ─ ─ ─ ─

┌────────────────┐      ┌────────────────┐      ┌────────────────┐
│ CLAUDE-CODE    │      │ CODEX          │      │ GEMINI-CLI     │
│ inbox: 0       │      │ inbox: 1 ← +1  │      │ inbox: 1 ← +1  │
└────────┬───────┘      └────────▲───────┘      └────────▲───────┘
         │ 5. consolidate, ask for sign-off              │
         ●━━━━━━━━━━━━━━━━━━━━━━━┘                       │
         ●━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┘

  Liveness sidecar (metadata only):
            ┌────────────────┐
            │ WATCHDOG       │   if nobody acks for a while:
            │ liveness §10   │   ─ ─ ─ ─▶ pokes the stale peer
            └────────────────┘   (uses inbox metadata, not bodies)`,
];

// padFrames normalizes every frame in a sequence to the same line count, by
// appending blank lines to shorter frames. This keeps the rendered <pre>
// block's height stable across frames so the page below doesn't reflow on
// every step.
function padFrames(frames) {
  const lineCounts = frames.map((f) => f.split('\n').length);
  const maxLines = Math.max(...lineCounts);
  return frames.map((f) => {
    const lines = f.split('\n');
    while (lines.length < maxLines) lines.push('');
    return lines.join('\n');
  });
}

const ANIMATIONS = {
  handoff: { frames: padFrames(HANDOFF_FRAMES), intervalMs: 3800 },
  review: { frames: padFrames(REVIEW_FRAMES), intervalMs: 4800 },
};

function createAnimation(rootEl) {
  const name = rootEl.dataset.anim;
  const spec = ANIMATIONS[name];
  if (!spec) return;

  const pre = rootEl.querySelector('pre.ascii-anim > code');
  const stepLabel = rootEl.querySelector('.anim-step-label');
  const playBtn = rootEl.querySelector('.anim-play');

  let index = 0;
  let timerId = null;
  let playing = true;

  const setFrame = (i) => {
    index = ((i % spec.frames.length) + spec.frames.length) % spec.frames.length;
    pre.textContent = spec.frames[index];
    stepLabel.textContent = `step ${index + 1} / ${spec.frames.length}`;
  };

  const advance = () => setFrame(index + 1);

  const start = () => {
    if (timerId) return;
    timerId = setInterval(advance, spec.intervalMs);
    playing = true;
    playBtn.textContent = '⏸ Pause';
    playBtn.setAttribute('aria-label', 'Pause');
  };

  const stop = () => {
    if (!timerId) return;
    clearInterval(timerId);
    timerId = null;
    playing = false;
    playBtn.textContent = '▶ Play';
    playBtn.setAttribute('aria-label', 'Play');
  };

  rootEl.addEventListener('click', (e) => {
    const btn = e.target.closest('button[data-action]');
    if (!btn) return;
    const action = btn.dataset.action;
    if (action === 'toggle') {
      playing ? stop() : start();
    } else if (action === 'step-fwd') {
      stop();
      setFrame(index + 1);
    } else if (action === 'step-back') {
      stop();
      setFrame(index - 1);
    } else if (action === 'restart') {
      setFrame(0);
      start();
    }
  });

  // Respect reduced-motion preference.
  const prefersReduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  setFrame(0);
  if (prefersReduced) {
    stop();
  } else {
    // Pause when scrolled out of view (intersection observer).
    if ('IntersectionObserver' in window) {
      const io = new IntersectionObserver(
        (entries) => {
          for (const entry of entries) {
            if (entry.isIntersecting) {
              if (playing) start();
            } else {
              if (timerId) clearInterval(timerId), (timerId = null);
            }
          }
        },
        { threshold: 0.25 }
      );
      io.observe(rootEl);
    } else {
      start();
    }
  }
}

document.addEventListener('DOMContentLoaded', () => {
  document.querySelectorAll('.anim').forEach(createAnimation);
});
