'use strict';

/**
 * Entire Continuity — OpenClaw hook shim.
 *
 * Translates OpenClaw lifecycle callbacks into the `openclaw/v1` envelope and
 * appends them to an NDJSON log. It interprets nothing: mapping onto the
 * normalized event vocabulary happens once, for every host, in
 * internal/normalize.
 *
 * Register with OpenClaw's plugin API and forward the lifecycle points it
 * exposes. Points OpenClaw does not expose are simply not emitted — inventing
 * one would put a lifecycle semantic into task state that never happened.
 */

const fs = require('fs');
const path = require('path');

const HOOKS = Object.freeze({
  SESSION_START: 'session_start',
  SESSION_END: 'session_end',
  SESSION_INTERRUPT: 'session_interrupt',
  TURN_START: 'turn_start',
  TURN_END: 'turn_end',
  TOOL_USE: 'tool_use',
  SUBAGENT_START: 'subagent_start',
  SUBAGENT_END: 'subagent_end',
  CHECKPOINT: 'checkpoint',
});

/** Summaries are short by contract; a transcript belongs in an artifact. */
const MAX_SUMMARY = 500;

class OpenClawAdapter {
  /**
   * @param {object} opts
   * @param {string} opts.taskId    Task these events belong to (required).
   * @param {string} [opts.logFile] Defaults to .entire-continuity/events/openclaw.ndjson
   * @param {string} [opts.cwd]     Repository root.
   */
  constructor(opts = {}) {
    if (!opts.taskId) {
      throw new Error('OpenClawAdapter: taskId is required — an event that names no task cannot be filed');
    }
    this.taskId = opts.taskId;
    const cwd = opts.cwd || process.cwd();
    this.logFile = opts.logFile || path.join(cwd, '.entire-continuity', 'events', 'openclaw.ndjson');
  }

  /**
   * Append one envelope. Best-effort by design: continuity capture must never
   * take down the agent it is observing.
   */
  emit(hook, payload = {}) {
    if (!Object.values(HOOKS).includes(hook)) return false;
    const session = payload.session || {};
    const tool = payload.tool || {};

    const envelope = {
      hook,
      ts: (payload.timestamp ? new Date(payload.timestamp) : new Date()).toISOString(),
      session: {
        id: session.id || '',
        parent: session.parent || '',
        role: session.role || '',
      },
      task: { id: this.taskId },
      // The tool's name and a reference to its artifact — never the payload
      // itself. This is the first line of defence against a credential
      // reaching a checkpoint; the normalizer's redaction is the second.
      tool: { name: tool.name || '', ref: tool.ref || '' },
      summary: truncate(payload.summary || '', MAX_SUMMARY),
    };
    if (payload.meta && typeof payload.meta === 'object') {
      envelope.meta = scalarsOnly(payload.meta);
    }

    try {
      fs.mkdirSync(path.dirname(this.logFile), { recursive: true });
      fs.appendFileSync(this.logFile, JSON.stringify(envelope) + '\n', 'utf8');
      return true;
    } catch {
      // Deliberately swallowed. A failed capture is a gap the ingest side will
      // report; a thrown error here would break the host's own session.
      return false;
    }
  }

  sessionStarted(session, summary) {
    return this.emit(HOOKS.SESSION_START, { session, summary });
  }
  sessionEnded(session) {
    return this.emit(HOOKS.SESSION_END, { session });
  }
  sessionInterrupted(session, summary) {
    return this.emit(HOOKS.SESSION_INTERRUPT, { session, summary });
  }
  turnStarted(session, summary) {
    return this.emit(HOOKS.TURN_START, { session, summary });
  }
  turnEnded(session, summary, meta) {
    return this.emit(HOOKS.TURN_END, { session, summary, meta });
  }
  toolUsed(session, tool, summary, meta) {
    return this.emit(HOOKS.TOOL_USE, { session, tool, summary, meta });
  }
  /** `session` is the CHILD session; `session.parent` names the delegator. */
  subagentStarted(session, summary) {
    return this.emit(HOOKS.SUBAGENT_START, { session, summary });
  }
  subagentEnded(session, summary) {
    return this.emit(HOOKS.SUBAGENT_END, { session, summary });
  }
  checkpointCreated(session, checkpointId, summary) {
    return this.emit(HOOKS.CHECKPOINT, {
      session,
      tool: { ref: checkpointId },
      summary,
      meta: { checkpoint_id: checkpointId },
    });
  }

  /** Register against an OpenClaw plugin host. */
  register(host) {
    if (!host || typeof host.on !== 'function') {
      throw new Error('OpenClawAdapter.register: host does not expose an on() subscription API');
    }
    host.on('session:start', (e) => this.sessionStarted(e.session, e.prompt));
    host.on('session:end', (e) => this.sessionEnded(e.session));
    host.on('session:interrupt', (e) => this.sessionInterrupted(e.session, e.reason));
    host.on('turn:start', (e) => this.turnStarted(e.session, e.prompt));
    host.on('turn:end', (e) => this.turnEnded(e.session, e.summary, e.meta));
    host.on('tool:use', (e) => this.toolUsed(e.session, e.tool, e.summary, e.meta));
    host.on('subagent:start', (e) => this.subagentStarted(e.session, e.purpose));
    host.on('subagent:end', (e) => this.subagentEnded(e.session, e.summary));
    host.on('checkpoint:created', (e) => this.checkpointCreated(e.session, e.checkpointId, e.label));
    return this;
  }
}

function truncate(s, n) {
  const t = String(s == null ? '' : s).replace(/\s+/g, ' ').trim();
  return t.length <= n ? t : t.slice(0, n - 1) + '…';
}

/**
 * Attrs are documented as a small set of scalar fields. A nested value is a
 * payload, and a payload travels by reference, not by copy.
 */
function scalarsOnly(meta) {
  const out = {};
  for (const [k, v] of Object.entries(meta)) {
    if (v === null || v === undefined) continue;
    const t = typeof v;
    if (t === 'string' || t === 'number' || t === 'boolean') out[k] = v;
  }
  return out;
}

module.exports = { OpenClawAdapter, HOOKS };
