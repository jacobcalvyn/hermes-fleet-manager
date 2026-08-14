import { expect, test } from '@playwright/test'
import { buildProcessTimeline } from '../src/hermesTimeline'
import type { ChatEvent } from '../src/types'

function activity(id: number, sequence: number, event: string, data: Record<string, unknown>): ChatEvent {
	return {
		version: 1,
		id,
		session_id: 'session-1',
		operation_id: 'operation-1',
		sequence,
		type: 'ASSISTANT_ACTIVITY',
		payload: { kind: 'activity', event, data: JSON.stringify(data) },
		created_at: '2026-08-13T00:00:00Z',
	}
}

test('Waiting for response is a placeholder, never a timeline activity', () => {
	const waiting = buildProcessTimeline([], true)
	expect(waiting.showWaiting).toBe(true)
	expect(waiting.activities).toEqual([])

	const metadata = activity(1, 1, 'message', { choices: [] })
	const stillWaiting = buildProcessTimeline([metadata], true)
	expect(stillWaiting.showWaiting).toBe(true)
	expect(stillWaiting.activities).toEqual([])

	const started = activity(2, 2, 'tool.started', {
		type: 'tool.started',
		tool: { name: 'browser_snapshot', call_id: 'call-1' },
	})
	const timeline = buildProcessTimeline([started, metadata], true)
	expect(timeline.showWaiting).toBe(false)
	expect(timeline.activityCount).toBe(1)
	expect(timeline.activities[0].label).toBe('browser_snapshot')
	expect(timeline.activities.some((item) => item.label === 'Waiting for response')).toBe(false)

	const completed = activity(3, 3, 'tool.completed', {
		type: 'tool.completed',
		tool: { name: 'browser_snapshot', call_id: 'call-1' },
	})
	const waitingForNextFrame = buildProcessTimeline([started, completed], true)
	expect(waitingForNextFrame.showWaiting).toBe(true)
	expect(waitingForNextFrame.activityCount).toBe(1)
})

test('Hermes lifecycle updates one call without changing first-seen order', () => {
	const events = [
		activity(4, 4, 'tool.completed', { type: 'tool.completed', tool: { call_id: 'call-1' }, duration_ms: 1200 }),
		activity(2, 2, 'tool.started', { type: 'tool.started', tool: { name: 'second_tool', call_id: 'call-2' } }),
		activity(1, 1, 'tool.started', { type: 'tool.started', tool: { name: 'first_tool', call_id: 'call-1' }, arguments: { path: '/tmp/report' } }),
		activity(3, 3, 'tool.completed', { type: 'tool.completed', tool: { call_id: 'call-2', status: 'completed' } }),
	]
	const timeline = buildProcessTimeline(events, true)
	expect(timeline.activities.map((item) => item.label)).toEqual(['first_tool', 'second_tool'])
	expect(timeline.activities.map((item) => item.status)).toEqual(['completed', 'completed'])
	expect(timeline.activities[0].argumentsText).toBe('{"path":"/tmp/report"}')
	expect(timeline.activities[0].durationMs).toBe(1200)
})

test('Hermes session tool events preserve complete arguments beyond the preview', () => {
	const fullCommand = "from hermes_tools import write_file\nimport urllib.request,csv,io,json,re,time\nwrite_file('/root/dashboard_redirect.html', '<html></html>')\nprint('all commands retained')"
	const started = activity(1, 1, 'tool.started', {
		message_id: 'msg-1',
		tool_name: 'terminal',
		preview: 'from hermes_tools import write_file import urllib.request,csv,io,json,re,time + 3 commands',
		args: { command: fullCommand },
		run_id: 'run-1',
		seq: 3,
	})
	const completed = activity(2, 2, 'tool.completed', {
		message_id: 'msg-1',
		tool_name: 'terminal',
		preview: 'from hermes_tools import write_file import urllib.request,csv,io,json,re,time + 3 commands',
		args: { command: fullCommand },
		run_id: 'run-1',
		seq: 4,
	})
	const timeline = buildProcessTimeline([started, completed], true)
	expect(timeline.activityCount).toBe(1)
	expect(timeline.activities[0].label).toBe('terminal')
	expect(timeline.activities[0].status).toBe('completed')
	expect(timeline.activities[0].argumentsText).toBe(JSON.stringify({ command: fullCommand }))
	expect(timeline.activities[0].argumentsText).toContain("write_file('/root/dashboard_redirect.html'")
	expect(timeline.activities[0].argumentsText).toContain("print('all commands retained')")
})

test('Hermes _thinking progress is reasoning telemetry, not a fake tool', () => {
	const thinking = activity(1, 1, 'tool.progress', {
		message_id: 'msg-1',
		tool_name: '_thinking',
		delta: 'Inspecting the dashboard state',
		run_id: 'run-1',
		seq: 3,
	})
	const timeline = buildProcessTimeline([thinking], true)
	expect(timeline.activityCount).toBe(1)
	expect(timeline.activities[0]).toMatchObject({
		kind: 'reasoning',
		label: 'Thinking',
		status: 'recorded',
		detailText: 'Inspecting the dashboard state',
	})
	expect(timeline.activities[0].label).not.toBe('_thinking')
	expect(timeline.liveLabel).toBe('Thinking')
	expect(timeline.showWaiting).toBe(true)
})

test('argument deltas are accumulated on their call and never promoted to steps', () => {
	const events = [
		activity(1, 1, 'tool.started', { type: 'tool.started', tool: { name: 'browser_type', call_id: 'call-3' } }),
		activity(2, 2, 'tool.delta', { type: 'tool.delta', tool: { call_id: 'call-3' }, arguments: '{"value":"upt ' }),
		activity(3, 3, 'tool.delta', { type: 'tool.delta', tool: { call_id: 'call-3' }, arguments: 'posind3m4s"}' }),
		activity(4, 4, 'tool.completed', { type: 'tool.completed', tool: { call_id: 'call-3' } }),
		activity(5, 5, 'tool.delta', { type: 'tool.delta', tool: { name: 'orphan_tool', call_id: 'call-orphan' }, arguments: 'orphan fragment' }),
	]
	const timeline = buildProcessTimeline(events, true)
	expect(timeline.activityCount).toBe(1)
	expect(timeline.activities[0].label).toBe('browser_type')
	expect(timeline.activities[0].argumentsText).toBe('{"value":"upt posind3m4s"}')
})

test('a stopped stream does not synthesize tool completion', () => {
	const started = activity(1, 1, 'tool.started', {
		type: 'tool.started',
		tool: { name: 'browser_snapshot', call_id: 'call-1' },
	})
	const timeline = buildProcessTimeline([started], false)
	expect(timeline.activities[0].status).toBe('interrupted')
	expect(timeline.status).toBe('interrupted')
})

test('a recovered tool failure does not mark a completed Hermes run incomplete', () => {
	const events = [
		activity(1, 1, 'tool.started', { tool_name: 'browser_click', args: { ref: 'e1' } }),
		activity(2, 2, 'tool.failed', { tool_name: 'browser_click', args: { ref: 'e1' } }),
		activity(3, 3, 'tool.started', { tool_name: 'browser_console', args: { expression: 'document.body.innerText' } }),
		activity(4, 4, 'tool.completed', { tool_name: 'browser_console', args: { expression: 'document.body.innerText' } }),
	]
	// Completed snapshots retain rich events while message status is the
	// authoritative terminal outcome.
	const timeline = buildProcessTimeline(events, false, 'SUCCEEDED')
	expect(timeline.activities[0].label).toBe('browser_click')
	expect(timeline.activities[0].status).toBe('failed')
	expect(timeline.status).toBe('completed')
})
