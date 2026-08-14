import { isValidElement, useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type ReactNode, type UIEvent } from 'react'
import {
	AssistantRuntimeProvider,
	AuiIf,
	ComposerPrimitive,
	MessagePrimitive,
	ThreadPrimitive,
	type AppendMessage,
	type ThreadMessageLike,
	useAuiState,
	useExternalStoreRuntime,
} from '@assistant-ui/react'
import { ArrowDown, ArrowUp, Boxes, Check, ChevronDown, CircleAlert, CircleStop, Copy, Download, ExternalLink, FileAudio, FileText, FileVideo, Image, LoaderCircle, MessageCircle, Send } from 'lucide-react'
import ReactMarkdown, { type Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { buildProcessTimeline, type ProcessActivity } from './hermesTimeline'
import { apiDownloadToFile, apiRequest } from './api'
import type { ChatEvent, ChatMessage, DataArtifactPreview } from './types'

export type OptimisticChatMessage = {
	id: string
	content: string
	createdAt: string
	operationID?: string
}

type FleetRuntimeMessage = {
	id: string
	role: 'user' | 'assistant'
	content: string
	createdAt: string
	status: ChatMessage['status']
	error?: string
	optimistic?: boolean
	streaming?: boolean
	responseStartedAt?: string
	responseDurationMs?: number
	events: ChatEvent[]
}

type FleetChatMessageMetadata = {
	fleetStatus?: ChatMessage['status']
	fleetError?: string
	streaming?: boolean
	responseStartedAt?: string
	responseDurationMs?: number
	events?: ChatEvent[]
}

type FleetAssistantThreadProps = {
	messages: ChatMessage[]
	events: ChatEvent[]
	optimisticMessage: OptimisticChatMessage | null
	liveResponse: { operationID: string; content: string } | null
	responseInProgress: boolean
	sending: boolean
	canceling: boolean
	instanceName: string
	token: string
	formatTimestamp: (timestamp: string, now?: number) => string
	onSend: (content: string) => Promise<void>
	onCancel: () => Promise<void>
}

function formatActivityDuration(milliseconds?: number) {
	if (!milliseconds || milliseconds < 1) return ''
	if (milliseconds < 1000) return `${milliseconds}ms`
	const seconds = milliseconds / 1000
	return seconds < 10 ? `${seconds.toFixed(1)}s` : `${Math.round(seconds)}s`
}

function FleetProcessIcon() {
	return <span className="chat-process-fleet-icon" aria-hidden="true"><Boxes size={13} /></span>
}

function HermesProcessIcon({ className = '' }: { className?: string }) {
	return <span className={`chat-process-hermes-icon${className ? ` ${className}` : ''}`} aria-hidden="true"><img src="/hermes-logo.png" alt="" /></span>
}

function HermesRunningMarker() {
	return <span className="chat-process-marker chat-process-active-marker">
		<span className="sr-only">Hermes is working: </span>
		<HermesProcessIcon />
		<LoaderCircle size={13} className="spin" aria-hidden="true" />
	</span>
}

function ProcessActivityRow({ activity, active, current }: { activity: ProcessActivity; active: boolean; current: boolean }) {
	const running = active && activity.status === 'running'
	const showHermesStatus = running && current
	const Icon = activity.status === 'failed' ? CircleAlert : activity.status === 'completed' ? Check : MessageCircle
	return <div className={`chat-process-row chat-process-${activity.status}${showHermesStatus ? ' chat-process-current' : ''}`}>
		{showHermesStatus ? <HermesRunningMarker /> : <span className="chat-process-marker"><Icon size={13} /></span>}
		<span className="chat-process-content">
			<span className="chat-process-label">{activity.label}</span>
			{activity.argumentsText ? <code className="chat-process-arguments">{activity.argumentsText}</code> : null}
			{activity.detailText ? <span className="chat-process-reasoning-text">{activity.detailText}</span> : null}
		</span>
		{!running && activity.durationMs ? <span className="chat-process-duration">{formatActivityDuration(activity.durationMs)}</span> : null}
	</div>
}

function HermesActiveRow({ label }: { label: 'Thinking' | 'Working' }) {
	return <div className="chat-process-row chat-process-running chat-process-current chat-process-active-step">
		<HermesRunningMarker />
		<span className="chat-process-content">
			<span className="chat-process-active-label">{label}</span>
		</span>
	</div>
}

function useStableDisclosurePosition<T extends HTMLElement>(expanded: boolean, disclosureSelector: string) {
	const triggerRef = useRef<T>(null)
	const anchorRef = useRef<{ top: number; viewport: HTMLElement; disclosure: HTMLElement } | null>(null)
	const capturePosition = useCallback(() => {
		const trigger = triggerRef.current
		const viewport = trigger?.closest<HTMLElement>('.chat-transcript')
		const disclosure = trigger?.closest<HTMLElement>(disclosureSelector)
		if (!trigger || !viewport || !disclosure) return
		anchorRef.current = { top: trigger.getBoundingClientRect().top, viewport, disclosure }
	}, [disclosureSelector])

	useLayoutEffect(() => {
		const anchor = anchorRef.current
		if (!anchor) return
		let frame = 0
		let released = false
		const restore = () => {
			if (released) return
			const trigger = triggerRef.current
			if (!trigger?.isConnected) return
			const offset = trigger.getBoundingClientRect().top - anchor.top
			if (Math.abs(offset) > 0.25) anchor.viewport.scrollTop += offset
		}
		const scheduleRestore = () => {
			window.cancelAnimationFrame(frame)
			frame = window.requestAnimationFrame(restore)
		}
		const observer = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(() => {
			scheduleRestore()
		})
		const release = () => {
			released = true
			anchorRef.current = null
		}
		observer?.observe(anchor.disclosure)
		anchor.viewport.addEventListener('scroll', scheduleRestore)
		anchor.viewport.addEventListener('pointerdown', release, { once: true })
		anchor.viewport.addEventListener('wheel', release, { once: true })
		anchor.viewport.addEventListener('touchstart', release, { once: true })
		anchor.viewport.addEventListener('keydown', release, { once: true })
		const releaseTimer = window.setTimeout(release, 10000)
		scheduleRestore()
		return () => {
			released = true
			window.cancelAnimationFrame(frame)
			window.clearTimeout(releaseTimer)
			observer?.disconnect()
			anchor.viewport.removeEventListener('scroll', scheduleRestore)
			anchor.viewport.removeEventListener('pointerdown', release)
			anchor.viewport.removeEventListener('wheel', release)
			anchor.viewport.removeEventListener('touchstart', release)
			anchor.viewport.removeEventListener('keydown', release)
		}
	}, [expanded])

	return { triggerRef, capturePosition }
}

function ProcessTimeline({ events, streaming, messageStatus }: {
	events: ChatEvent[]
	streaming: boolean
	messageStatus?: ChatMessage['status']
}) {
	const [expanded, setExpanded] = useState(false)
	const activityBodyID = useId()
	const { triggerRef: disclosureTriggerRef, capturePosition } = useStableDisclosurePosition<HTMLButtonElement>(expanded, '.chat-process-card')
	const timeline = buildProcessTimeline(events, streaming, messageStatus)
	const phaseListRef = useRef<HTMLDivElement>(null)
	const lastEventID = events.at(-1)?.id
	useEffect(() => {
		if (!streaming || !phaseListRef.current) return
		phaseListRef.current.scrollTop = phaseListRef.current.scrollHeight
	}, [lastEventID, streaming, timeline.activities.length])
	if (timeline.activities.length === 0 && !timeline.showWaiting) return null
	const latestRunningActivity = [...timeline.activities].reverse().find((activity) => activity.status === 'running')
	const activities = <div ref={phaseListRef} className="chat-process-timeline">
		{timeline.activities.map((activity) => <ProcessActivityRow key={`${activity.key}:${activity.status}`} activity={activity} active={streaming} current={activity.key === latestRunningActivity?.key} />)}
		{timeline.showWaiting ? <HermesActiveRow label={timeline.liveLabel} /> : null}
	</div>
	if (streaming) {
		return <section className="chat-process-card chat-process-card-live" role="status" aria-live="polite">
			<div className="chat-process-card-heading">
				<span className="chat-process-card-title"><FleetProcessIcon /><span className="chat-process-live-label">Waiting for Hermes</span></span>
				<span className="chat-process-card-meta">{timeline.activityCount} {timeline.activityCount === 1 ? 'activity' : 'activities'}</span>
			</div>
			{activities}
		</section>
	}
	const needsAttention = timeline.status === 'failed' || timeline.status === 'interrupted'
	return <section className={`chat-process-card chat-process-card-${timeline.status}${expanded ? ' chat-process-card-expanded' : ''}`}>
		<button ref={disclosureTriggerRef} type="button" className="chat-process-card-heading" aria-controls={activityBodyID} aria-expanded={expanded} onClick={() => {
			capturePosition()
			setExpanded((current) => !current)
		}}>
			<span className="chat-process-card-title">{needsAttention ? <CircleAlert size={14} /> : <FleetProcessIcon />}<span>{timeline.activityCount} {timeline.activityCount === 1 ? 'activity' : 'activities'}</span><ChevronDown size={13} className="chat-process-chevron" /></span>
		</button>
		<div id={activityBodyID} className={`chat-process-body${expanded ? ' chat-process-body-open' : ''}`} aria-hidden={!expanded}>
			<div className="chat-process-body-inner">{activities}</div>
		</div>
	</section>
}

type ChatArtifact = NonNullable<NonNullable<ChatEvent['payload']>['artifact']>

function formatArtifactSize(size?: number) {
	if (!size || size < 1) return ''
	if (size < 1024) return `${size} B`
	if (size < 1024 * 1024) return `${(size / 1024).toFixed(size < 10240 ? 1 : 0)} KiB`
	return `${(size / (1024 * 1024)).toFixed(size < 10 * 1024 * 1024 ? 1 : 0)} MiB`
}

function ArtifactIcon({ kind }: { kind: ChatArtifact['kind'] }) {
	switch (kind) {
	case 'image': return <Image size={17} />
	case 'audio': return <FileAudio size={17} />
	case 'video': return <FileVideo size={17} />
	default: return <FileText size={17} />
	}
}

function messageArtifacts(events: ChatEvent[]) {
	const order: string[] = []
	const artifacts = new Map<string, ChatArtifact>()
	for (const event of [...events].sort((left, right) => left.sequence - right.sequence || left.id - right.id)) {
		const artifact = event.type === 'ASSISTANT_ARTIFACT' ? event.payload?.artifact : undefined
		if (!artifact) continue
		const key = artifact.id || artifact.url || `${artifact.kind}:${artifact.name}:${artifact.media_type ?? ''}`
		if (!artifacts.has(key)) order.push(key)
		artifacts.set(key, artifact)
	}
	return order.flatMap((key) => {
		const artifact = artifacts.get(key)
		return artifact ? [artifact] : []
	})
}

type GridPoint = { row: number; column: number }

type DisplayPreviewRow = {
	values: string[]
	rowNumber: number
	stableIndex: number
}

function isDataArtifact(artifact: ChatArtifact) {
	const mediaType = artifact.media_type?.toLowerCase().split(';')[0]
	return mediaType === 'text/csv' || mediaType === 'text/plain' ||
		mediaType === 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet' ||
		/\.(csv|txt|xlsx)$/i.test(artifact.name)
}

function spreadsheetSafeValue(value: string) {
	const leading = value.trimStart()
	if (!/^[=+\-@]/.test(leading) || /^-\d+(?:\.\d+)?$/.test(leading)) return value
	return `'${value}`
}

function quoteDelimited(value: string, delimiter: ',' | '\t') {
	const safe = spreadsheetSafeValue(value)
	return safe.includes(delimiter) || /["\r\n]/.test(safe) ? `"${safe.replaceAll('"', '""')}"` : safe
}

function copyText(value: string) {
	if (navigator.clipboard?.writeText) return navigator.clipboard.writeText(value)
	const textarea = document.createElement('textarea')
	textarea.value = value
	textarea.setAttribute('readonly', '')
	textarea.style.position = 'fixed'
	textarea.style.opacity = '0'
	document.body.appendChild(textarea)
	textarea.select()
	const copied = document.execCommand('copy')
	textarea.remove()
	return copied ? Promise.resolve() : Promise.reject(new Error('Clipboard is unavailable'))
}

function gridColumnName(index: number) {
	let value = index + 1
	let name = ''
	while (value > 0) {
		value--
		name = String.fromCharCode(65 + value % 26) + name
		value = Math.floor(value / 26)
	}
	return name
}

function artifactColumnWidth(header: string, values: string[]) {
	const longestValue = values.reduce((longest, value) => Math.max(longest, Array.from(value).length), Array.from(header).length)
	return Math.max(52, Math.min(320, Math.ceil(longestValue * 6.2) + 22))
}

function artifactSheetLabel(sheet: string, index: number) {
	if (index === 0) return 'Data Preview'
	if (/^(informasi|info|metadata)$/i.test(sheet.trim())) return 'File Info'
	return sheet
}

const artifactPreviewRowLimit = 50

function ArtifactDataViewer({ artifact, manifestURL, token }: {
	artifact: ChatArtifact
	manifestURL: string
	token: string
}) {
	const rootRef = useRef<HTMLDivElement>(null)
	const gridRef = useRef<HTMLDivElement>(null)
	const previewBodyID = useId()
	const [visible, setVisible] = useState(false)
	const [requestedSheet, setRequestedSheet] = useState('')
	const [preview, setPreview] = useState<DataArtifactPreview | null>(null)
	const [error, setError] = useState('')
	const [sort, setSort] = useState<{ column: number; direction: 'ascending' | 'descending' } | null>(null)
	const [anchor, setAnchor] = useState<GridPoint | null>(null)
	const [focus, setFocus] = useState<GridPoint | null>(null)
	const [dragging, setDragging] = useState(false)
	const [previewOpen, setPreviewOpen] = useState(false)
	const [gridViewportWidth, setGridViewportWidth] = useState(0)
	const [copyStatus, setCopyStatus] = useState('')
	const { triggerRef: previewToolbarRef, capturePosition: capturePreviewPosition } = useStableDisclosurePosition<HTMLButtonElement>(previewOpen, '.chat-artifact-data')

	useEffect(() => {
		const element = rootRef.current
		if (!element || typeof IntersectionObserver === 'undefined') {
			setVisible(true)
			return
		}
		const observer = new IntersectionObserver((entries) => {
			if (!entries.some((entry) => entry.isIntersecting)) return
			setVisible(true)
			observer.disconnect()
		}, { rootMargin: '240px' })
		observer.observe(element)
		return () => observer.disconnect()
	}, [])

	useEffect(() => {
		if (!visible || !manifestURL) return
		const controller = new AbortController()
		const params = requestedSheet ? `?${new URLSearchParams({ sheet: requestedSheet }).toString()}` : ''
		void apiRequest<DataArtifactPreview>(token, `${manifestURL}/preview${params}`, {
			signal: controller.signal,
		}).then((data) => {
			setPreview(data)
			setSort(null)
			setAnchor(null)
			setFocus(null)
		}).catch((reason: unknown) => {
			if (controller.signal.aborted) return
			setError(reason instanceof Error ? reason.message : 'Preview unavailable')
		})
		return () => controller.abort()
	}, [manifestURL, requestedSheet, token, visible])

	useLayoutEffect(() => {
		const element = gridRef.current
		if (!element || !preview) {
			setGridViewportWidth(0)
			return
		}
		const updateWidth = () => setGridViewportWidth(element.clientWidth)
		updateWidth()
		if (typeof ResizeObserver === 'undefined') {
			window.addEventListener('resize', updateWidth)
			return () => window.removeEventListener('resize', updateWidth)
		}
		const observer = new ResizeObserver(updateWidth)
		observer.observe(element)
		return () => observer.disconnect()
	}, [preview])

	useEffect(() => {
		const stopDragging = () => setDragging(false)
		window.addEventListener('pointerup', stopDragging)
		return () => window.removeEventListener('pointerup', stopDragging)
	}, [])

	useEffect(() => {
		if (!copyStatus) return
		const timer = window.setTimeout(() => setCopyStatus(''), 1800)
		return () => window.clearTimeout(timer)
	}, [copyStatus])

	const previewRows = useMemo(() => preview?.rows.slice(0, artifactPreviewRowLimit) ?? [], [preview])

	const rows = useMemo<DisplayPreviewRow[]>(() => {
		if (!preview) return []
		const values = previewRows.map((row, stableIndex) => ({
			values: row,
			rowNumber: preview.row_numbers?.[stableIndex] ?? stableIndex + 1,
			stableIndex,
		}))
		if (!sort) return values
		return [...values].sort((left, right) => {
			const comparison = (left.values[sort.column] ?? '').localeCompare(right.values[sort.column] ?? '', undefined, {
				numeric: true, sensitivity: 'base',
			})
			return sort.direction === 'ascending' ? comparison : -comparison
		})
	}, [preview, previewRows, sort])

	const bounds = useMemo(() => anchor && focus ? {
		rowStart: Math.min(anchor.row, focus.row),
		rowEnd: Math.max(anchor.row, focus.row),
		columnStart: Math.min(anchor.column, focus.column),
		columnEnd: Math.max(anchor.column, focus.column),
	} : null, [anchor, focus])

	const selectionText = (includeHeaders: boolean, delimiter: ',' | '\t') => {
		if (!preview || !bounds) return ''
		const output: string[] = []
		if (includeHeaders) {
			output.push(preview.columns.slice(bounds.columnStart, bounds.columnEnd + 1)
				.map((value) => quoteDelimited(value, delimiter)).join(delimiter))
		}
		for (let rowIndex = bounds.rowStart; rowIndex <= bounds.rowEnd; rowIndex++) {
			const row = rows[rowIndex]
			if (!row) continue
			output.push(row.values.slice(bounds.columnStart, bounds.columnEnd + 1)
				.map((value) => quoteDelimited(value ?? '', delimiter)).join(delimiter))
		}
		return output.join('\n')
	}

	const copySelection = async (includeHeaders: boolean) => {
		const value = selectionText(includeHeaders, '\t')
		if (!value) return
		try {
			await copyText(value)
			setCopyStatus(includeHeaders ? 'Copied with headers' : 'Copied')
		} catch {
			setCopyStatus('Copy failed')
		}
	}

	const exportSelection = () => {
		const value = selectionText(true, ',')
		if (!value) return
		const blob = new Blob([`\uFEFF${value}\n`], { type: 'text/csv;charset=utf-8' })
		const url = URL.createObjectURL(blob)
		const link = document.createElement('a')
		link.href = url
		link.download = `${artifact.name.replace(/\.[^.]+$/, '') || 'artifact'}-selection.csv`
		document.body.appendChild(link)
		link.click()
		link.remove()
		URL.revokeObjectURL(url)
	}

	const moveFocus = (rowDelta: number, columnDelta: number, extend: boolean) => {
		if (!preview || rows.length === 0 || preview.columns.length === 0) return
		const current = focus ?? { row: 0, column: 0 }
		const next = {
			row: Math.max(0, Math.min(rows.length - 1, current.row + rowDelta)),
			column: Math.max(0, Math.min(preview.columns.length - 1, current.column + columnDelta)),
		}
		setFocus(next)
		if (!extend || !anchor) setAnchor(next)
	}

	const selectedValue = focus && rows[focus.row] ? rows[focus.row].values[focus.column] ?? '' : ''
	const selectedReference = focus && rows[focus.row] ? `${gridColumnName(focus.column)}${rows[focus.row].rowNumber}` : ''
	const truncated = Boolean(preview && (preview.truncated_rows || preview.truncated_columns || preview.truncated_cells || preview.rows.length > previewRows.length))
	const loading = visible && !preview && !error
	const columnWidths = useMemo(() => preview ? preview.columns.map((column, columnIndex) =>
		artifactColumnWidth(column, previewRows.map((row) => row[columnIndex] ?? ''))) : [], [preview, previewRows])
	const displayColumnWidths = useMemo(() => {
		if (columnWidths.length === 0) return columnWidths
		const widths = [...columnWidths]
		const naturalWidth = 46 + widths.reduce((total, width) => total + width, 0)
		widths[widths.length - 1] += Math.max(0, gridViewportWidth - naturalWidth)
		return widths
	}, [columnWidths, gridViewportWidth])
	const tableWidth = 46 + displayColumnWidths.reduce((total, width) => total + width, 0)
	const totalRows = preview?.total_rows && preview.total_rows > 0 ? preview.total_rows : previewRows.length
	const rowSummary = preview ? preview.total_rows_exact
			? `Preview ${previewRows.length} dari ${totalRows} baris`
			: `Preview ${previewRows.length} baris`
		: ''

	return <div ref={rootRef} className={`chat-artifact-data${previewOpen ? ' chat-artifact-data-expanded' : ''}`}>
		<button ref={previewToolbarRef} type="button" className="chat-artifact-data-toolbar" onClick={() => {
			capturePreviewPosition()
			setPreviewOpen((current) => {
				if (current) {
					setAnchor(null)
					setFocus(null)
				}
				return !current
			})
		}} aria-controls={previewBodyID} aria-expanded={previewOpen} aria-label={previewOpen ? 'Hide artifact preview' : 'Show artifact preview'}>
			<span className="chat-artifact-data-label">Data preview</span>
			<span className="chat-artifact-data-actions">
				{preview && <span>{rowSummary}</span>}
				<ChevronDown className="chat-artifact-data-chevron" size={14} aria-hidden="true" />
			</span>
		</button>
		<div id={previewBodyID} className={`chat-artifact-data-body${previewOpen ? ' chat-artifact-data-body-open' : ''}`} aria-hidden={!previewOpen}>
		<div className="chat-artifact-data-body-inner">
		{preview && (preview.sheets?.length ?? 0) > 1 && <div className="chat-artifact-data-controls">
			<div className="chat-artifact-sheet-buttons" role="group" aria-label="Worksheet">
				{preview.sheets?.map((sheet, index) => {
					const active = sheet === (requestedSheet || preview.sheet || '')
					return <button key={`${sheet}:${index}`} type="button" className={active ? 'chat-artifact-sheet-active' : ''} aria-pressed={active} onClick={() => {
						if (active || loading) return
						setError('')
						setPreview(null)
						setRequestedSheet(sheet)
					}} disabled={loading}>{artifactSheetLabel(sheet, index)}</button>
				})}
			</div>
		</div>}
		{bounds && <div className="chat-artifact-selection-actions" aria-live="polite">
			<span>{copyStatus || `${bounds.rowEnd - bounds.rowStart + 1} × ${bounds.columnEnd - bounds.columnStart + 1} selected`}</span>
			<button type="button" onClick={() => void copySelection(false)}><Copy size={12} />Copy</button>
			<button type="button" onClick={() => void copySelection(true)}><Copy size={12} />Copy headers</button>
			<button type="button" onClick={exportSelection}><Download size={12} />Export CSV</button>
		</div>}
		{loading && !preview && <div className="chat-artifact-data-state"><LoaderCircle size={15} className="spin" />Loading data preview</div>}
		{error && !preview && <div className="chat-artifact-data-state chat-artifact-data-error"><CircleAlert size={15} />Preview unavailable. The original file can still be downloaded.</div>}
		{preview && <>
			<div
				ref={gridRef}
				className="chat-artifact-grid-scroll"
				tabIndex={0}
				role="region"
				aria-label={`Interactive preview of ${artifact.name}`}
				onKeyDown={(event) => {
					if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'c' && bounds) {
						event.preventDefault()
						void copySelection(false)
						return
					}
					const moves: Record<string, [number, number]> = { ArrowUp: [-1, 0], ArrowDown: [1, 0], ArrowLeft: [0, -1], ArrowRight: [0, 1] }
					const move = moves[event.key]
					if (!move) return
					event.preventDefault()
					moveFocus(move[0], move[1], event.shiftKey)
				}}
			>
				<table className="chat-artifact-grid" style={{ width: `${tableWidth}px`, minWidth: `${tableWidth}px` }}>
					<colgroup>
						<col className="chat-artifact-grid-row-number-column" />
						{displayColumnWidths.map((width, columnIndex) => <col key={columnIndex} style={{ width: `${width}px` }} />)}
					</colgroup>
					<thead><tr>
						<th className="chat-artifact-grid-row-number" scope="col">#</th>
						{preview.columns.map((column, columnIndex) => <th key={`${column}:${columnIndex}`} scope="col">
							<button type="button" onClick={() => {
								setSort((current) => current?.column === columnIndex
									? { column: columnIndex, direction: current.direction === 'ascending' ? 'descending' : 'ascending' }
									: { column: columnIndex, direction: 'ascending' })
								setAnchor(null)
								setFocus(null)
							}}>
								<span>{column}</span>
								{sort?.column === columnIndex ? sort.direction === 'ascending' ? <ArrowUp size={11} /> : <ArrowDown size={11} /> : null}
							</button>
						</th>)}
					</tr></thead>
					<tbody>{rows.map((row, rowIndex) => <tr key={`${row.stableIndex}:${row.rowNumber}`}>
						<th className="chat-artifact-grid-row-number" scope="row">{row.rowNumber}</th>
						{preview.columns.map((_, columnIndex) => {
							const selected = bounds && rowIndex >= bounds.rowStart && rowIndex <= bounds.rowEnd && columnIndex >= bounds.columnStart && columnIndex <= bounds.columnEnd
							const focused = focus?.row === rowIndex && focus.column === columnIndex
							return <td
								key={columnIndex}
								className={`${selected ? 'chat-artifact-cell-selected' : ''}${focused ? ' chat-artifact-cell-focused' : ''}`}
								aria-selected={selected || undefined}
								onPointerDown={(event) => {
									event.preventDefault()
									const point = { row: rowIndex, column: columnIndex }
									if (!event.shiftKey || !anchor) setAnchor(point)
									setFocus(point)
									setDragging(true)
									gridRef.current?.focus()
								}}
								onPointerEnter={() => {
									if (dragging) setFocus({ row: rowIndex, column: columnIndex })
								}}
							><span>{row.values[columnIndex] ?? ''}</span></td>
						})}
					</tr>)}</tbody>
				</table>
				{rows.length === 0 && <div className="chat-artifact-data-empty">No rows are available in this preview.</div>}
			</div>
			{focus && <div className="chat-artifact-cell-inspector"><strong>{selectedReference}</strong><code>{selectedValue || 'Empty cell'}</code></div>}
			<div className="chat-artifact-data-note">
				<span>Read-only preview</span>
				{truncated && <span>Showing a bounded sample; sorting applies to loaded rows.</span>}
				{preview.truncated_cells && <span>Long cell values are shortened here; download preserves the original.</span>}
			</div>
		</>}
		</div>
		</div>
	</div>
}

function ArtifactCard({ artifact, token }: { artifact: ChatArtifact; token: string }) {
	const [manifestArtifact, setManifestArtifact] = useState<ChatArtifact | null>(null)
	const resolvedArtifact = manifestArtifact && manifestArtifact.id === artifact.id ? manifestArtifact : artifact
	const internalURL = resolvedArtifact.url?.startsWith('/api/v1/chats/') ? resolvedArtifact.url : ''
	const manifestURL = internalURL.endsWith('/download') ? internalURL.slice(0, -'/download'.length) : ''
	const [previewURL, setPreviewURL] = useState('')
	const [busy, setBusy] = useState(false)
	const [actionError, setActionError] = useState('')
	useEffect(() => {
		if (!manifestURL) return
		const controller = new AbortController()
		void apiRequest<ChatArtifact>(token, manifestURL, { signal: controller.signal })
			.then((current) => setManifestArtifact(current))
			.catch(() => undefined)
		return () => controller.abort()
	}, [manifestURL, token])
	useEffect(() => {
		if (resolvedArtifact.kind !== 'image' || !internalURL || resolvedArtifact.status !== 'ready') return
		const controller = new AbortController()
		let objectURL = ''
		void fetch(internalURL, {
			headers: { Authorization: `Bearer ${token}` }, cache: 'no-store', signal: controller.signal,
		}).then(async (response) => {
			if (!response.ok) throw new Error('Preview unavailable')
			const blob = await response.blob()
			if (!blob.type.startsWith('image/') || blob.size > 25 * 1024 * 1024) throw new Error('Preview unavailable')
			objectURL = URL.createObjectURL(blob)
			setPreviewURL(objectURL)
		}).catch(() => undefined)
		return () => {
			controller.abort()
			if (objectURL) URL.revokeObjectURL(objectURL)
		}
	}, [resolvedArtifact.kind, resolvedArtifact.status, internalURL, token])
	const metadata = [resolvedArtifact.media_type, formatArtifactSize(resolvedArtifact.size_bytes), resolvedArtifact.source_tool ? `via ${resolvedArtifact.source_tool}` : '']
		.filter(Boolean)
		.join(' · ')
	const download = async () => {
		if (!internalURL || busy) return
		setBusy(true)
		setActionError('')
		try {
			await apiDownloadToFile(token, internalURL, resolvedArtifact.name)
		} catch (error) {
			setActionError(error instanceof Error ? error.message : 'Download failed')
		} finally {
			setBusy(false)
		}
	}
	const status = resolvedArtifact.status ?? 'ready'
	const unavailable = ['failed', 'rejected', 'missing', 'expired'].includes(status)
	const preparing = status === 'preparing'
	const unavailableLabel = status === 'expired' ? 'Expired' : status === 'missing' ? 'Missing' : status === 'rejected' ? 'Rejected' : 'Unavailable'
	return <div className="chat-artifact-block"><div className={`chat-artifact-card chat-artifact-status-${status}${unavailable ? ' chat-artifact-failed' : ''}`}>
		{previewURL
			? <a className="chat-artifact-preview" href={previewURL} target="_blank" rel="noreferrer" aria-label={`Open ${resolvedArtifact.name}`}><img src={previewURL} alt="" /></a>
			: <span className={`chat-artifact-icon chat-artifact-${resolvedArtifact.kind}`}>{preparing ? <LoaderCircle size={17} className="spin" /> : <ArtifactIcon kind={resolvedArtifact.kind} />}</span>}
		<span className="chat-artifact-copy">
			<strong>{resolvedArtifact.name}</strong>
			{metadata && <small>{metadata}</small>}
			{(resolvedArtifact.error || actionError) && <small className="chat-artifact-error">{resolvedArtifact.error || actionError}</small>}
		</span>
		{internalURL && status === 'ready'
			? <button className="chat-artifact-action" type="button" onClick={() => void download()} disabled={busy}><Download size={14} /><span>{busy ? 'Saving' : 'Download'}</span></button>
			: resolvedArtifact.url && status === 'ready'
				? <a className="chat-artifact-action" href={resolvedArtifact.url} target="_blank" rel="noreferrer"><ExternalLink size={14} /><span>Open</span></a>
				: <span className="chat-artifact-unavailable">{preparing ? 'Preparing' : unavailableLabel}</span>}
	</div>
		{manifestURL && status === 'ready' && isDataArtifact(resolvedArtifact) && <ArtifactDataViewer artifact={resolvedArtifact} manifestURL={manifestURL} token={token} />}
	</div>
}

function ArtifactOutputs({ events, token }: { events: ChatEvent[]; token: string }) {
	const artifacts = messageArtifacts(events)
	if (artifacts.length === 0) return null
	return <div className="chat-artifact-list" aria-label="Outputs">
		{artifacts.map((artifact) => <ArtifactCard key={artifact.id || artifact.url || `${artifact.kind}:${artifact.name}`} artifact={artifact} token={token} />)}
	</div>
}

function convertFleetChatMessage(message: FleetRuntimeMessage): ThreadMessageLike {
	return {
		id: message.id,
		role: message.role,
		content: message.content ? [{ type: 'text', text: message.content }] : [],
		createdAt: new Date(message.createdAt),
		...(message.role === 'assistant' ? {
			status: message.streaming
				? { type: 'running' as const }
				: message.status === 'FAILED'
					? { type: 'incomplete' as const, reason: 'error' as const, error: message.error ?? 'Response failed' }
					: { type: 'complete' as const, reason: 'stop' as const },
		} : {}),
		metadata: {
			custom: {
				fleetStatus: message.status,
				fleetError: message.error,
				streaming: message.streaming,
				responseStartedAt: message.responseStartedAt,
				responseDurationMs: message.responseDurationMs,
				events: message.events,
			},
			...(message.optimistic ? { isOptimistic: true } : {}),
		},
	}
}

function chatMessageText(message: AppendMessage) {
	return message.content
		.filter((part): part is Extract<(typeof message.content)[number], { type: 'text' }> => part.type === 'text')
		.map((part) => part.text)
		.join('')
		.trim()
}

function formatResponseDuration(milliseconds: number) {
	const seconds = Math.max(0, Math.round(milliseconds / 1000))
	if (seconds < 1) return '<1s'
	if (seconds < 60) return `${seconds}s`
	const minutes = Math.floor(seconds / 60)
	const remainingSeconds = seconds % 60
	return remainingSeconds > 0 ? `${minutes}m ${remainingSeconds}s` : `${minutes}m`
}

function fullTimestamp(timestamp: string) {
	const value = new Date(timestamp)
	if (Number.isNaN(value.getTime())) return timestamp
	return new Intl.DateTimeFormat('en-GB', {
		day: '2-digit',
		month: 'short',
		year: 'numeric',
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit',
		timeZoneName: 'short',
	}).format(value)
}

function MarkdownCodeBlock({ code, language }: { code: string; language?: string }) {
	const [copied, setCopied] = useState(false)
	const resetTimer = useRef<number | null>(null)
	useEffect(() => () => {
		if (resetTimer.current !== null) window.clearTimeout(resetTimer.current)
	}, [])
	const copyCode = async () => {
		if (!navigator.clipboard) return
		try {
			await navigator.clipboard.writeText(code)
			setCopied(true)
			if (resetTimer.current !== null) window.clearTimeout(resetTimer.current)
			resetTimer.current = window.setTimeout(() => setCopied(false), 1500)
		} catch {
			setCopied(false)
		}
	}
	return <div className="chat-code-block">
		<div className="chat-code-heading">
			<span>{language || 'code'}</span>
			<button type="button" onClick={copyCode} aria-label={copied ? 'Code copied' : 'Copy code'} title={copied ? 'Copied' : 'Copy code'}>
				{copied ? <Check size={12} /> : <Copy size={12} />}
				<span>{copied ? 'Copied' : 'Copy'}</span>
			</button>
		</div>
		<pre><code className={language ? `language-${language}` : undefined}>{code}</code></pre>
	</div>
}

const markdownComponents: Components = {
	a: ({ href, children, ...props }) => <a {...props} href={href} target="_blank" rel="noopener noreferrer">{children}</a>,
	pre: ({ children }) => {
		if (!isValidElement<{ className?: string; children?: ReactNode }>(children)) return <pre>{children}</pre>
		const language = children.props.className?.match(/language-([^\s]+)/)?.[1]
		const code = String(children.props.children ?? '').replace(/\n$/, '')
		return <MarkdownCodeBlock code={code} language={language} />
	},
}

function ChatMarkdown({ children }: { children: string }) {
	return <div className="chat-markdown">
		<ReactMarkdown
			remarkPlugins={[remarkGfm]}
			components={markdownComponents}
			skipHtml
			disallowedElements={['img']}
			unwrapDisallowed
		>
			{children}
		</ReactMarkdown>
	</div>
}

function FleetChatMessageView({ formatTimestamp, onReuse, token }: {
	formatTimestamp: (timestamp: string, now?: number) => string
	onReuse: (content: string) => void
	token: string
}) {
	const message = useAuiState((state) => state.message)
	const metadata = message.metadata.custom as FleetChatMessageMetadata
	const text = message.parts
		.filter((part): part is Extract<(typeof message.parts)[number], { type: 'text' }> => part.type === 'text')
		.map((part) => part.text)
		.join('')
	const failed = metadata.fleetStatus === 'FAILED'
	const richEvents = metadata.events ?? []
	const timestamp = message.createdAt.toISOString()
	const [copied, setCopied] = useState(false)
	const [clock, setClock] = useState(() => Date.now())
	const copyResetTimer = useRef<number | null>(null)
	useEffect(() => {
		const createdAt = message.createdAt.getTime()
		if (!metadata.streaming && Date.now() - createdAt >= 60000) return
		const timer = window.setInterval(() => {
			const now = Date.now()
			setClock(now)
			if (!metadata.streaming && now - createdAt >= 60000) window.clearInterval(timer)
		}, 1000)
		return () => window.clearInterval(timer)
	}, [message.createdAt, metadata.streaming])
	useEffect(() => () => {
		if (copyResetTimer.current !== null) window.clearTimeout(copyResetTimer.current)
	}, [])
	const copyMessage = async () => {
		if (!text || !navigator.clipboard) return
		try {
			await navigator.clipboard.writeText(text)
			setCopied(true)
			if (copyResetTimer.current !== null) window.clearTimeout(copyResetTimer.current)
			copyResetTimer.current = window.setTimeout(() => setCopied(false), 1500)
		} catch {
			setCopied(false)
		}
	}
	return <MessagePrimitive.Root className={`chat-bubble chat-${message.role}${failed ? ' chat-failed' : ''}${metadata.streaming ? ' chat-streaming' : ''}`}>
		{message.role === 'assistant' && <ProcessTimeline events={richEvents} streaming={Boolean(metadata.streaming)} messageStatus={metadata.fleetStatus} />}
		{text ? message.role === 'assistant'
			? <div className="chat-assistant-response"><span className="sr-only">Hermes response: </span><HermesProcessIcon className="chat-response-hermes-icon" /><ChatMarkdown>{text}</ChatMarkdown></div>
			: <ChatMarkdown>{text}</ChatMarkdown>
			: null}
		{message.role === 'assistant' && <ArtifactOutputs events={richEvents} token={token} />}
		{failed && <div className="chat-message-error"><span>{metadata.fleetError || 'Hermes did not complete this message.'}</span><button className="text-button" type="button" onClick={() => onReuse(text)}>Use again</button></div>}
		<div className="chat-bubble-footer">
			{text && <button className="chat-copy-button" type="button" onClick={copyMessage} aria-label={copied ? 'Message copied' : 'Copy message'} title={copied ? 'Copied' : 'Copy message'}>{copied ? <Check size={12} /> : <Copy size={12} />}</button>}
			{message.role === 'assistant' && metadata.streaming && metadata.responseStartedAt
				? <span className="chat-response-duration">{formatResponseDuration(clock - new Date(metadata.responseStartedAt).getTime())}</span>
				: <>{message.role === 'assistant' && metadata.responseDurationMs !== undefined && <><span className="chat-response-duration">{formatResponseDuration(metadata.responseDurationMs)}</span><span className="chat-footer-separator" aria-hidden="true">·</span></>}<time dateTime={timestamp} title={fullTimestamp(timestamp)}>{formatTimestamp(timestamp, clock)}</time></>}
		</div>
	</MessagePrimitive.Root>
}

export default function FleetAssistantThread({
	messages,
	events,
	optimisticMessage,
	liveResponse,
	responseInProgress,
	sending,
	canceling,
	instanceName,
	token,
	formatTimestamp,
	onSend,
	onCancel,
}: FleetAssistantThreadProps) {
	const composerInputRef = useRef<HTMLTextAreaElement>(null)
	const transcriptRef = useRef<HTMLDivElement>(null)
	const followStreamingRef = useRef(true)
	const previousResponseInProgress = useRef(false)
	useEffect(() => {
		const frame = window.requestAnimationFrame(() => {
			composerInputRef.current?.focus({ preventScroll: true })
			const viewport = transcriptRef.current
			if (viewport) viewport.scrollTop = viewport.scrollHeight
		})
		return () => window.cancelAnimationFrame(frame)
	}, [])
	useLayoutEffect(() => {
		if (responseInProgress && !previousResponseInProgress.current) followStreamingRef.current = true
		previousResponseInProgress.current = responseInProgress
		if (!responseInProgress || !followStreamingRef.current) return
		const frame = window.requestAnimationFrame(() => {
			const viewport = transcriptRef.current
			if (viewport) viewport.scrollTop = viewport.scrollHeight
		})
		return () => window.cancelAnimationFrame(frame)
	}, [events.length, liveResponse?.content, responseInProgress])
	const trackTranscriptPosition = useCallback((event: UIEvent<HTMLDivElement>) => {
		const viewport = event.currentTarget
		followStreamingRef.current = viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight <= 16
	}, [])
	const runtimeMessages = useMemo<FleetRuntimeMessage[]>(() => {
		const usersByAssistantID = new Map(messages
			.filter((message) => message.role === 'user')
			.map((message) => [`${message.id}-assistant`, message]))
		const operationsByAssistantID = new Map(messages
			.filter((message) => message.role === 'user' && message.operation_id)
			.map((message) => [`${message.id}-assistant`, message.operation_id as string]))
		const result: FleetRuntimeMessage[] = messages.map((message) => {
			const operationID = message.operation_id || operationsByAssistantID.get(message.id)
			const requestMessage = message.role === 'assistant' ? usersByAssistantID.get(message.id) : undefined
			const responseDurationMs = requestMessage
				? Math.max(0, new Date(message.created_at).getTime() - new Date(requestMessage.created_at).getTime())
				: undefined
			return {
				id: message.id,
				role: message.role,
				content: message.content,
				createdAt: message.created_at,
				status: message.status,
				error: message.error,
				responseStartedAt: requestMessage?.created_at,
				responseDurationMs,
				events: message.role === 'assistant' && operationID
					? events.filter((event) => event.operation_id === operationID)
					: [],
			}
		})
		const optimisticPersisted = optimisticMessage?.operationID
			? messages.some((message) => message.operation_id === optimisticMessage.operationID)
			: false
		if (optimisticMessage && !optimisticPersisted) {
			result.push({
				id: optimisticMessage.id,
				role: 'user',
				content: optimisticMessage.content,
				createdAt: optimisticMessage.createdAt,
				status: 'PENDING',
				optimistic: true,
				events: [],
			})
		}
		if (responseInProgress) {
			const requestMessage = liveResponse?.operationID
				? [...messages].reverse().find((message) => message.role === 'user' && message.operation_id === liveResponse.operationID)
				: [...messages].reverse().find((message) => message.role === 'user' && message.status === 'PENDING')
			const responseStartedAt = requestMessage?.created_at || optimisticMessage?.createdAt || new Date().toISOString()
			result.push({
				id: `live-${liveResponse?.operationID || optimisticMessage?.id || 'response'}`,
				role: 'assistant',
				content: liveResponse?.content ?? '',
				createdAt: responseStartedAt,
				status: 'PENDING',
				streaming: true,
				responseStartedAt,
				events: events.filter((event) => event.operation_id === liveResponse?.operationID),
			})
		}
		return result
	}, [events, liveResponse, messages, optimisticMessage, responseInProgress])

	const runtime = useExternalStoreRuntime({
		messages: runtimeMessages,
		convertMessage: convertFleetChatMessage,
		isRunning: responseInProgress,
		isSendDisabled: sending,
		onNew: async (message) => {
			const content = chatMessageText(message)
			if (content) await onSend(content)
		},
		onCancel,
	})
	const reuseMessage = useCallback((content: string) => {
		runtime.thread.composer.setText(content)
	}, [runtime])
	const Message = useCallback(() => <FleetChatMessageView formatTimestamp={formatTimestamp} onReuse={reuseMessage} token={token} />, [formatTimestamp, reuseMessage, token])

	return <AssistantRuntimeProvider runtime={runtime}>
		<ThreadPrimitive.Root className="chat-thread-runtime">
			<ThreadPrimitive.Viewport ref={transcriptRef} className="chat-transcript" autoScroll={false} scrollToBottomOnInitialize={false} scrollToBottomOnRunStart={false} scrollToBottomOnThreadSwitch={false} onScroll={trackTranscriptPosition}>
				<ThreadPrimitive.Empty><div className="chat-empty-thread"><MessageCircle size={26} /><strong>Start this conversation</strong></div></ThreadPrimitive.Empty>
				<ThreadPrimitive.Messages components={{ Message }} />
			</ThreadPrimitive.Viewport>
			<ComposerPrimitive.Root className="chat-composer">
				<ComposerPrimitive.Input ref={composerInputRef} autoFocus aria-label="Message" placeholder={`Message ${instanceName}`} submitMode="enter" disabled={sending} />
				<AuiIf condition={(state) => !state.thread.isRunning}><ComposerPrimitive.Send className="primary-button"><Send size={16} /><span>Send</span></ComposerPrimitive.Send></AuiIf>
				<AuiIf condition={(state) => state.thread.isRunning}><ComposerPrimitive.Cancel className="danger-button chat-stop-button" disabled={canceling}><CircleStop size={16} /><span>{canceling ? 'Stopping' : 'Stop'}</span></ComposerPrimitive.Cancel></AuiIf>
				<span>Enter to send · Shift+Enter for a new line</span>
			</ComposerPrimitive.Root>
		</ThreadPrimitive.Root>
	</AssistantRuntimeProvider>
}
