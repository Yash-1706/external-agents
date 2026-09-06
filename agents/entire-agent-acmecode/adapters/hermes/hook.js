'use strict';

/**
 * Entire Continuity — Hermes hook shim.
 *
 * Emits the `hermes/v1` envelope. Hermes names its lifecycle points differently
 * from OpenClaw and stamps time as epoch milliseconds; both differences are
 * preserved here verbatim and reconciled once, in internal/normalize. That is
 * the whole demonstration: two hosts, two wire shapes, one task-state engine.
 *
 * Note what is absent. Hermes exposes no child-agent *start* hook, so this shim
 * emits no equivalent of subagent_start. A shim that synthesised one would be
 * putting a lifecycle point into task state that the host never reported.
 */

const fs = require('fs');
const path = require('path');

const EVENTS = Object.freeze({
  SESSION_START: 'SessionStart',
  SESSION_END: 'SessionEnd',
  SESSION_ABORTED: 'SessionAborted',
  PRE_LLM: 'PreLLM',
  POST_LLM: 'PostLLM',
  TOOL_INVOKED: 'ToolInvoked',
  CHILD_AGENT_FINISHED: 'ChildAgentFinished',
  CHECKPOINT_WRITTEN: 'CheckpointWritten',
});

const MAX_NOTE = 500;

class HermesAdapter {
  /**
   * @param {object} opts
   * @param {string} opts.taskId    Task these events belong to (required).
   * @param {string} [opts.logFile] Defaults to .entire-continuity/events/hermes.ndjson
   * @param {string} [opts.cwd]     Repository root.
   */
  constructor(opts = {}) {
    if (!opts.taskId) {
      throw new Error('HermesAdapter: taskId is required — an event that names no task cannot be filed');
    }
    this.taskId = opts.taskId;
    const cwd = opts.cwd || process.cwd();
    this.logFile = opts.logFile || path.join(cwd, '.entire-continuity', 'events', 'hermes.ndjson');
  }

  emit(event, payload = {}) {
    if (!Object.values(EVENTS).includes(event)) return false;

    const envelope = {
      event,
      // Epoch milliseconds is Hermes's own encoding. Normalizing it here would
      // move host-specific knowledge out of the one place that owns it.
      time: payload.timestamp ? new Date(payload.timestamp).getTime() : Date.now(),
      agent_session: payload.session || '',
      delegated_from: payload.parent || '',
      agent_role: payload.role || '',
      entire_task: this.taskId,
      payload_ref: payload.ref || '',
      note: truncate(payload.note || '', MAX_NOTE),
    };
    if (payload.fields && typeof payload.fields === 'object') {
      envelope.fields = scalarsOnly(payload.fields);
    }

    try {
      fs.mkdirSync(path.dirname(this.logFile), { recursive: true });
      fs.appendFileSync(this.logFile, JSON.stringify(envelope) + '\n', 'utf8');
      return true;
    } catch {
      return false; // never take the host down over a capture failure
    }
  }

  sessionStart(session, note, role) {
    return this.emit(EVENTS.SESSION_START, { session, note, role });
  }
  sessionEnd(session) {
    return this.emit(EVENTS.SESSION_END, { session });
  }
  sessionAborted(session, note) {
    return this.emit(EVENTS.SESSION_ABORTED, { session, note });
  }
  preLLM(session, note) {
    return this.emit(EVENTS.PRE_LLM, { session, note });
  }
  postLLM(session, note, fields) {
    return this.emit(EVENTS.POST_LLM, { session, note, fields });
  }
  toolInvoked(session, toolName, ref, note) {
    return this.emit(EVENTS.TOOL_INVOKED, { session, ref, note, fields: { tool_name: toolName } });
  }
  /** `session` is the CHILD session; `parent` names the delegating session. */
  childAgentFinished(session, parent, note) {
    return this.emit(EVENTS.CHILD_AGENT_FINISHED, { session, parent, note });
  }
  checkpointWritten(session, checkpointId, note) {
    return this.emit(EVENTS.CHECKPOINT_WRITTEN, {
      session,
      ref: checkpointId,
      note,
      fields: { checkpoint_id: checkpointId },
    });
  }

  /** Register against a Hermes plugin host. */
  register(host) {
    if (!host || typeof host.hook !== 'function') {
      throw new Error('HermesAdapter.register: host does not expose a hook() registration API');
    }
    host.hook('session.start', (e) => this.sessionStart(e.sessionId, e.prompt, e.role));
    host.hook('session.end', (e) => this.sessionEnd(e.sessionId));
    host.hook('session.abort', (e) => this.sessionAborted(e.sessionId, e.reason));
    host.hook('llm.pre', (e) => this.preLLM(e.sessionId, e.prompt));
    host.hook('llm.post', (e) => this.postLLM(e.sessionId, e.summary, e.fields));
    host.hook('tool.invoke', (e) => this.toolInvoked(e.sessionId, e.tool, e.ref, e.summary));
    host.hook('agent.child.done', (e) => this.childAgentFinished(e.childId, e.parentId, e.summary));
    host.hook('checkpoint.write', (e) => this.checkpointWritten(e.sessionId, e.checkpointId, e.label));
    return this;
  }
}

function truncate(s, n) {
  const t = String(s == null ? '' : s).replace(/\s+/g, ' ').trim();
  return t.length <= n ? t : t.slice(0, n - 1) + '…';
}

function scalarsOnly(fields) {
  const out = {};
  for (const [k, v] of Object.entries(fields)) {
    if (v === null || v === undefined) continue;
    const t = typeof v;
    if (t === 'string' || t === 'number' || t === 'boolean') out[k] = v;
  }
  return out;
}

module.exports = { HermesAdapter, EVENTS };
