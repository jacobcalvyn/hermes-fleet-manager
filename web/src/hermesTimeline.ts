import type { ChatEvent } from './types'

export type ProcessStepStatus = 'recorded' | 'running' | 'completed' | 'failed' | 'interrupted'

export type ProcessActivity = {
	key: string
	kind: 'tool' | 'reasoning'
	label: string
	status: ProcessStepStatus
	callID?: string
	argumentsText?: string
	detailText?: string
	durationMs?: number
}

export type ProcessTimelineModel = {
	activities: ProcessActivity[]
	activityCount: number
	status: Exclude<ProcessStepStatus, 'recorded'>
	liveLabel: 'Thinking' | 'Working'
	showWaiting: boolean
}

type HermesEventData = Record<string, unknown>

type ParsedActivity = ProcessActivity & {
	labelPriority: number
	argumentDelta: boolean
	appendArguments: boolean
	toolName?: string
}

function parseHermesEventData(data?: string): unknown {
	if (!data) return undefined
	try {
		return JSON.parse(data)
	} catch {
		return data
	}
}

function objectField(value: unknown, key: string): unknown {
	return value && typeof value === 'object' && !Array.isArray(value)
		? (value as HermesEventData)[key]
		: undefined
}

function exactString(...values: unknown[]) {
	for (const value of values) {
		if (typeof value === 'string' && value.trim()) return value
	}
	return ''
}

function exactNumber(...values: unknown[]) {
	for (const value of values) {
		if (typeof value === 'number' && Number.isFinite(value) && value >= 0) return value
	}
	return undefined
}

function exactObject(...values: unknown[]): HermesEventData | undefined {
	for (const value of values) {
		if (value && typeof value === 'object' && !Array.isArray(value)) return value as HermesEventData
	}
	return undefined
}

function formatHermesArguments(value: unknown) {
	if (value === undefined || value === null) return ''
	if (typeof value === 'string') return value
	try {
		return JSON.stringify(value)
	} catch {
		return String(value)
	}
}

function activityStatus(eventName: string, value?: string): Exclude<ProcessStepStatus, 'interrupted'> {
	const explicitStatus = value?.trim().toLowerCase()
	if (explicitStatus === 'completed' || explicitStatus === 'succeeded') return 'completed'
	if (explicitStatus === 'failed' || explicitStatus === 'error') return 'failed'
	if (explicitStatus === 'running' || explicitStatus === 'started') return 'running'
	const eventState = eventName.trim().toLowerCase().split('.').at(-1)
	if (eventState === 'completed' || eventState === 'succeeded' || eventState === 'result') return 'completed'
	if (eventState === 'failed' || eventState === 'error') return 'failed'
	if (eventState === 'started' || eventState === 'running' || eventState === 'progress') return 'running'
	return 'recorded'
}

function processActivity(event: ChatEvent): ParsedActivity | null {
	const payload = event.payload
	if (!payload || event.type !== 'ASSISTANT_ACTIVITY') return null
	const data = parseHermesEventData(payload.data)
	const rawTool = objectField(data, 'tool')
	const toolData = exactObject(rawTool)
	const itemData = exactObject(objectField(data, 'item'))
	const functionData = exactObject(
		objectField(data, 'function'),
		objectField(toolData, 'function'),
		objectField(itemData, 'function'),
	)
	const rawType = exactString(objectField(data, 'type'))
	const itemType = exactString(objectField(itemData, 'type'))
	const eventName = exactString(rawType, payload.event)
	const eventKind = `${payload.event} ${rawType} ${itemType}`.trim().toLowerCase()
	const hasRawData = payload.data !== undefined && payload.data !== ''
	const hermesLabel = exactString(
		objectField(data, 'label'),
		objectField(toolData, 'label'),
		objectField(itemData, 'label'),
		objectField(functionData, 'label'),
	)
	const preview = exactString(
		objectField(data, 'preview'),
		objectField(toolData, 'preview'),
		objectField(itemData, 'preview'),
		objectField(functionData, 'preview'),
	)
	const toolName = exactString(
		objectField(data, 'tool_name'),
		typeof rawTool === 'string' ? rawTool : objectField(toolData, 'name'),
		objectField(itemData, 'name'),
		objectField(functionData, 'name'),
		hasRawData ? undefined : payload.tool,
	)
	const isReasoningFrame = toolName === '_thinking' || [
		'reasoning', 'thinking', 'analysis',
	].some((marker) => eventKind.includes(marker))
	if (isReasoningFrame) {
		const detailText = exactString(
			objectField(data, 'delta'),
			objectField(data, 'text'),
			objectField(data, 'reasoning'),
			objectField(data, 'reasoning_content'),
			preview,
			hermesLabel,
		)
		if (!detailText) return null
		return {
			key: `activity:event:${event.id}`,
			kind: 'reasoning',
			label: 'Thinking',
			labelPriority: 5,
			status: 'recorded',
			detailText,
			argumentDelta: false,
			appendArguments: false,
		}
	}
	const callID = exactString(
		objectField(data, 'call_id'), objectField(data, 'callId'), objectField(data, 'tool_call_id'), objectField(data, 'toolCallId'),
		objectField(toolData, 'call_id'), objectField(toolData, 'callId'), objectField(toolData, 'tool_call_id'), objectField(toolData, 'toolCallId'),
		objectField(itemData, 'call_id'), objectField(itemData, 'callId'), objectField(itemData, 'tool_call_id'), objectField(itemData, 'toolCallId'),
		objectField(functionData, 'call_id'), objectField(functionData, 'callId'), objectField(functionData, 'tool_call_id'), objectField(functionData, 'toolCallId'),
		itemType.includes('call') ? objectField(itemData, 'id') : undefined,
		hasRawData ? undefined : payload.call_id,
	)
	const isToolFrame = Boolean(toolName || callID) || [
		'tool.', 'tool_', 'tool-', 'function_call', 'function.call',
	].some((marker) => eventKind.includes(marker))
	if (!isToolFrame) return null
	const argumentDelta = eventKind.includes('delta')
	if (argumentDelta && !callID) return null
	const rawLabel = exactString(toolName, hermesLabel, preview, eventName)
	if (!rawLabel) return null
	const explicitStatus = exactString(
		objectField(data, 'status'),
		objectField(toolData, 'status'),
		objectField(itemData, 'status'),
		objectField(functionData, 'status'),
		hasRawData ? undefined : payload.status,
	)
	const milliseconds = exactNumber(
		objectField(data, 'duration_ms'), objectField(toolData, 'duration_ms'), objectField(itemData, 'duration_ms'), objectField(functionData, 'duration_ms'),
		hasRawData ? undefined : payload.duration_ms,
	)
	const seconds = exactNumber(
		objectField(data, 'duration'), objectField(data, 'duration_seconds'),
		objectField(toolData, 'duration'), objectField(toolData, 'duration_seconds'),
		objectField(itemData, 'duration'), objectField(itemData, 'duration_seconds'),
		objectField(functionData, 'duration'), objectField(functionData, 'duration_seconds'),
	)
	const argumentsValue = objectField(data, 'arguments') ?? objectField(data, 'args') ?? objectField(data, 'input') ?? objectField(data, 'parameters') ?? objectField(data, 'params')
		?? objectField(toolData, 'arguments') ?? objectField(toolData, 'args') ?? objectField(toolData, 'input') ?? objectField(toolData, 'parameters') ?? objectField(toolData, 'params')
		?? objectField(itemData, 'arguments') ?? objectField(itemData, 'args') ?? objectField(itemData, 'input') ?? objectField(itemData, 'parameters') ?? objectField(itemData, 'params')
		?? objectField(functionData, 'arguments') ?? objectField(functionData, 'args') ?? objectField(functionData, 'input') ?? objectField(functionData, 'parameters') ?? objectField(functionData, 'params')
	return {
		key: `activity:event:${event.id}`,
		kind: 'tool',
		label: rawLabel,
		labelPriority: toolName ? 4 : hermesLabel ? 3 : preview ? 2 : 1,
		status: activityStatus(eventName, explicitStatus),
		callID: callID || undefined,
		argumentsText: formatHermesArguments(argumentsValue) || undefined,
		argumentDelta,
		appendArguments: argumentDelta && typeof argumentsValue === 'string',
		toolName: toolName || undefined,
		durationMs: milliseconds ?? (seconds === undefined ? undefined : seconds * 1000),
	}
}

export function buildProcessTimeline(
	events: ChatEvent[],
	streaming: boolean,
	messageStatus?: 'PENDING' | 'SUCCEEDED' | 'FAILED',
): ProcessTimelineModel {
	const orderedActivities = events
		.filter((event) => event.payload && event.type === 'ASSISTANT_ACTIVITY')
		.sort((left, right) => left.sequence - right.sequence || left.id - right.id)
		.map(processActivity)
		.filter((activity): activity is ParsedActivity => Boolean(activity))
	const activities: ParsedActivity[] = []
	const activityByCallID = new Map<string, number>()
	const pendingArgumentsByCallID = new Map<string, string>()
	for (const activity of orderedActivities) {
		let existingIndex = activity.callID ? activityByCallID.get(activity.callID) : undefined
		if (existingIndex === undefined && !activity.callID && activity.toolName &&
			(activity.status === 'completed' || activity.status === 'failed')) {
			for (let index = activities.length - 1; index >= 0; index--) {
				const candidate = activities[index]
				if (!candidate.callID && candidate.toolName === activity.toolName && candidate.status === 'running') {
					existingIndex = index
					break
				}
			}
		}
		if (activity.argumentDelta && existingIndex === undefined) {
			if (activity.callID && activity.argumentsText) {
				pendingArgumentsByCallID.set(
					activity.callID,
					`${pendingArgumentsByCallID.get(activity.callID) ?? ''}${activity.argumentsText}`,
				)
			}
			continue
		}
		if (existingIndex === undefined) {
			const pendingArguments = activity.callID ? pendingArgumentsByCallID.get(activity.callID) : undefined
			activities.push({
				...activity,
				argumentsText: activity.argumentsText ?? pendingArguments,
			})
			if (activity.callID) activityByCallID.set(activity.callID, activities.length - 1)
			if (activity.callID) pendingArgumentsByCallID.delete(activity.callID)
			continue
		}
		const existing = activities[existingIndex]
		const argumentsText = activity.argumentsText
			? activity.appendArguments
				? `${existing.argumentsText ?? ''}${activity.argumentsText}`
				: activity.argumentsText
			: existing.argumentsText
		activities[existingIndex] = {
			...existing,
			label: activity.labelPriority > existing.labelPriority ? activity.label : existing.label,
			labelPriority: Math.max(existing.labelPriority, activity.labelPriority),
			status: activity.status === 'recorded' ? existing.status : activity.status,
			argumentsText,
			durationMs: activity.durationMs ?? existing.durationMs,
		}
	}
	if (!streaming) {
		for (const activity of activities) {
			if (activity.status === 'running') activity.status = 'interrupted'
		}
	}
	const activityCount = activities.length
	const hasRunningTool = activities.some((activity) => activity.kind === 'tool' && activity.status === 'running')
	const runCompleted = events.some((event) => event.type === 'RUN_COMPLETED')
	const runFailed = events.some((event) => event.type === 'RUN_FAILED')
	const runCanceled = events.some((event) => event.type === 'RUN_CANCELED')
	const status = streaming
		? 'running'
		: messageStatus === 'SUCCEEDED'
			? 'completed'
			: messageStatus === 'FAILED' || runFailed
			? 'failed'
			: runCanceled
				? 'interrupted'
				: runCompleted
					? 'completed'
					: activities.some((activity) => activity.status === 'failed')
						? 'failed'
						: activities.some((activity) => activity.status === 'interrupted')
							? 'interrupted'
							: 'completed'
	const liveLabel = hasRunningTool ? 'Working' : 'Thinking'
	return {
		activities: activities.map((activity) => ({
			key: activity.key,
			kind: activity.kind,
			label: activity.label,
			status: activity.status,
			callID: activity.callID,
			argumentsText: activity.argumentsText,
			detailText: activity.detailText,
			durationMs: activity.durationMs,
		})),
		activityCount,
		status,
		liveLabel,
		showWaiting: streaming && !hasRunningTool,
	}
}
