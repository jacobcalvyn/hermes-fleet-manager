import { type FormEvent, useCallback, useEffect, useState } from 'react'
import { Check, Download, FileOutput, LoaderCircle, MessageCircle, RefreshCw, Trash2, X } from 'lucide-react'
import { apiDownloadToFile, apiRequest } from './api'
import type { Instance, OutputArtifact, OutputArtifactPage, OutputStatus, OutputUsage } from './types'

type Filters = {
	query: string
	instanceID: string
	status: string
	kind: string
	time: string
}

const emptyFilters: Filters = { query: '', instanceID: '', status: '', kind: '', time: '' }

export default function OutputsView({ token, instances, refreshSignal, initialInstanceID = '', onOpenChat }: {
	token: string
	instances: Instance[]
	refreshSignal: number
	initialInstanceID?: string
	onOpenChat: (sessionID: string) => void
}) {
	const [draftQuery, setDraftQuery] = useState('')
	const [filters, setFilters] = useState<Filters>(() => ({ ...emptyFilters, instanceID: initialInstanceID }))
	const [timeAnchor, setTimeAnchor] = useState(() => Date.now())
	const [page, setPage] = useState<OutputArtifactPage>({ items: [] })
	const [usage, setUsage] = useState<OutputUsage | null>(null)
	const [cursorHistory, setCursorHistory] = useState<string[]>([])
	const [loading, setLoading] = useState(true)
	const [busyID, setBusyID] = useState('')
	const [deleteConfirmationID, setDeleteConfirmationID] = useState('')
	const [error, setError] = useState('')

	const load = useCallback(async (cursor = '') => {
		setLoading(true)
		setError('')
		const params = outputQuery(filters, cursor, timeAnchor)
		try {
			const [nextPage, nextUsage] = await Promise.all([
				apiRequest<OutputArtifactPage>(token, `/api/v1/artifacts?${params.toString()}`, { cache: 'no-store' }),
				apiRequest<OutputUsage>(token, '/api/v1/artifacts/usage', { cache: 'no-store' }),
			])
			setPage({ ...nextPage, items: nextPage.items ?? [] })
			setUsage(nextUsage)
		} catch (requestError) {
			setError(requestError instanceof Error ? requestError.message : 'Outputs could not be loaded')
		} finally {
			setLoading(false)
		}
	}, [filters, timeAnchor, token])

	useEffect(() => {
		const refresh = window.setTimeout(() => void load(cursorHistory[cursorHistory.length - 1] ?? ''), 0)
		return () => window.clearTimeout(refresh)
	}, [load, refreshSignal, cursorHistory])

	useEffect(() => {
		if (!deleteConfirmationID) return
		const close = (event: KeyboardEvent) => {
			if (event.key === 'Escape' && !busyID) setDeleteConfirmationID('')
		}
		window.addEventListener('keydown', close)
		return () => window.removeEventListener('keydown', close)
	}, [busyID, deleteConfirmationID])

	const applySearch = (event: FormEvent) => {
		event.preventDefault()
		setCursorHistory([])
		setFilters((current) => ({ ...current, query: draftQuery.trim() }))
	}
	const updateFilter = (key: keyof Filters, value: string) => {
		setCursorHistory([])
		if (key === 'time') setTimeAnchor(Date.now())
		setFilters((current) => ({ ...current, [key]: value }))
	}
	const resetFilters = () => {
		setDraftQuery('')
		setCursorHistory([])
		setTimeAnchor(Date.now())
		setFilters(emptyFilters)
	}
	const download = async (output: OutputArtifact) => {
		if (!output.download_url) return
		setBusyID(output.id)
		setError('')
		try {
			await apiDownloadToFile(token, output.download_url, output.name)
		} catch (downloadError) {
			setError(downloadError instanceof Error ? downloadError.message : 'Output could not be downloaded')
		} finally {
			setBusyID('')
		}
	}
	const remove = async (output: OutputArtifact) => {
		setBusyID(output.id)
		setError('')
		try {
			await apiRequest<OutputArtifact>(token, `/api/v1/artifacts/${encodeURIComponent(output.id)}`, { method: 'DELETE' })
			setDeleteConfirmationID('')
			await load(cursorHistory[cursorHistory.length - 1] ?? '')
		} catch (deleteError) {
			setError(deleteError instanceof Error ? deleteError.message : 'Output could not be deleted')
		} finally {
			setBusyID('')
		}
	}

	const counts = usage?.status_counts ?? {}
	const attention = (counts.rejected ?? 0) + (counts.missing ?? 0) + (counts.failed ?? 0)
	const storedPercent = usage?.total_max_bytes ? Math.min(100, (usage.total_bytes / usage.total_max_bytes) * 100) : 0
	const filtersActive = Object.values(filters).some(Boolean)

	return <section className="section-block first-section outputs-section">
		<div className="section-heading"><div><h2>Outputs</h2><p>Files produced by Hermes sessions and retained by Fleet</p></div><div className="button-row">{filtersActive && <button className="text-button" onClick={resetFilters}>Clear filters</button>}<button className="secondary-button compact-button" onClick={() => void load(cursorHistory[cursorHistory.length - 1] ?? '')} disabled={loading}><RefreshCw size={15} className={loading ? 'spin' : ''} />Refresh</button></div></div>
		<div className="output-summary-band" aria-label="Output storage summary">
			<div><span>Stored</span><strong>{formatBytes(usage?.total_bytes ?? 0)} / {formatBytes(usage?.total_max_bytes ?? 0)}</strong><i><b style={{ width: `${storedPercent}%` }} /></i></div>
			<div><span>Ready</span><strong>{counts.ready ?? 0}</strong><small>Available to download</small></div>
			<div><span>Needs attention</span><strong>{attention}</strong><small>Rejected, missing, or failed</small></div>
			<div><span>Retention</span><strong>{usage ? `${Math.round(usage.retention_hours / 24)} days` : '—'}</strong><small>Ready content only</small></div>
		</div>
		<form className="outputs-toolbar" onSubmit={applySearch}>
			<div className="output-search"><input aria-label="Search outputs" placeholder="Search name, type, ID, or error" value={draftQuery} onChange={(event) => setDraftQuery(event.target.value)} /><button className="secondary-button compact-button" type="submit">Search</button></div>
			<select aria-label="Filter outputs by instance" value={filters.instanceID} onChange={(event) => updateFilter('instanceID', event.target.value)}><option value="">All instances</option>{instances.map((instance) => <option key={instance.id} value={instance.id}>{instance.name}</option>)}</select>
			<select aria-label="Filter outputs by status" value={filters.status} onChange={(event) => updateFilter('status', event.target.value)}><option value="">All statuses</option>{['preparing', 'ready', 'rejected', 'missing', 'expired', 'failed', 'deleted'].map((status) => <option key={status} value={status}>{statusLabel(status as OutputStatus)}</option>)}</select>
			<select aria-label="Filter outputs by kind" value={filters.kind} onChange={(event) => updateFilter('kind', event.target.value)}><option value="">All types</option>{['file', 'image', 'audio', 'video'].map((kind) => <option key={kind} value={kind}>{capitalize(kind)}</option>)}</select>
			<select aria-label="Filter outputs by time" value={filters.time} onChange={(event) => updateFilter('time', event.target.value)}><option value="">Any time</option><option value="24H">Last 24 hours</option><option value="7D">Last 7 days</option><option value="30D">Last 30 days</option></select>
		</form>
		{error && <div className="inline-error output-error">{error}</div>}
		{loading && page.items.length === 0 ? <div className="empty-state"><LoaderCircle className="spin" size={22} /><strong>Loading outputs</strong></div> : page.items.length === 0 ? <div className="empty-state"><FileOutput size={24} /><strong>No outputs match these filters</strong><span>Hermes file, image, audio, and video outputs will appear here.</span></div> : <div className="table-wrap"><table className="provider-table outputs-table"><thead><tr><th>Output</th><th>Origin</th><th>Status</th><th>Created / retention</th><th><span className="sr-only">Actions</span></th></tr></thead><tbody>{page.items.map((output) => <tr key={output.id}>
			<td data-label="Output"><div className="output-name"><FileOutput size={17} /><div><strong>{output.name}</strong><span>{output.media_type || output.kind} · {output.size_bytes > 0 ? formatBytes(output.size_bytes) : 'No stored content'}</span></div></div></td>
			<td data-label="Origin"><strong>{output.instance_name || output.instance_id.slice(0, 8)}</strong><span className="secondary-text">{output.session_title || output.session_id.slice(0, 8)}</span><button className="output-origin-link" onClick={() => onOpenChat(output.session_id)}><MessageCircle size={13} />Open chat</button></td>
			<td data-label="Status"><OutputStatusBadge status={output.status} />{output.error && <span className="output-row-error">{output.error}</span>}</td>
			<td data-label="Created / retention">{relativeTime(output.created_at)}<span className="secondary-text">{output.deleted_at ? `Deleted ${relativeTime(output.deleted_at)}` : output.expires_at ? output.status === 'ready' ? `Expires ${relativeTimeFuture(output.expires_at)}` : `Retention ended ${relativeTime(output.expires_at)}` : 'No retention deadline'}</span></td>
			<td data-label="Actions"><div className="row-actions output-actions">{output.status === 'ready' && output.download_url && <button className="icon-button" title="Download output" aria-label={`Download ${output.name}`} onClick={() => void download(output)} disabled={busyID === output.id}>{busyID === output.id ? <LoaderCircle className="spin" size={15} /> : <Download size={15} />}</button>}{output.status !== 'deleted' && (deleteConfirmationID === output.id ? <><button className="icon-button danger-button" title="Confirm delete" aria-label={`Confirm delete ${output.name}`} onClick={() => void remove(output)} disabled={busyID === output.id}>{busyID === output.id ? <LoaderCircle className="spin" size={15} /> : <Check size={15} />}</button><button className="icon-button" title="Cancel delete" aria-label={`Cancel delete ${output.name}`} onClick={() => setDeleteConfirmationID('')} disabled={busyID === output.id}><X size={15} /></button></> : <button className="icon-button danger-button" title="Delete output" aria-label={`Delete ${output.name}`} onClick={() => setDeleteConfirmationID(output.id)} disabled={Boolean(busyID)}><Trash2 size={15} /></button>)}</div></td>
		</tr>)}</tbody></table></div>}
		{(cursorHistory.length > 0 || page.next_cursor) && <div className="pagination"><span>Page {cursorHistory.length + 1}</span><div><button className="secondary-button compact-button" onClick={() => setCursorHistory((current) => current.slice(0, -1))} disabled={cursorHistory.length === 0 || loading}>Previous</button><button className="secondary-button compact-button" onClick={() => page.next_cursor && setCursorHistory((current) => [...current, page.next_cursor as string])} disabled={!page.next_cursor || loading}>Next</button></div></div>}
	</section>
}

function outputQuery(filters: Filters, cursor: string, timeAnchor: number) {
	const params = new URLSearchParams({ limit: '25' })
	if (filters.query) params.set('q', filters.query)
	if (filters.instanceID) params.set('instance_id', filters.instanceID)
	if (filters.status) params.set('status', filters.status)
	if (filters.kind) params.set('kind', filters.kind)
	if (filters.time) {
		const duration = filters.time === '24H' ? 24 : filters.time === '7D' ? 7 * 24 : 30 * 24
		params.set('created_after', new Date(timeAnchor - duration * 60 * 60 * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z'))
	}
	if (cursor) params.set('cursor', cursor)
	return params
}

function OutputStatusBadge({ status }: { status: OutputStatus }) {
	return <span className={`status status-${status}`}><span />{statusLabel(status)}</span>
}

function statusLabel(status: OutputStatus) {
	if (status === 'preparing') return 'Preparing'
	if (status === 'ready') return 'Ready'
	if (status === 'rejected') return 'Rejected'
	if (status === 'missing') return 'Missing'
	if (status === 'expired') return 'Expired'
	if (status === 'failed') return 'Failed'
	return 'Deleted'
}

function relativeTime(timestamp: string) {
	const seconds = Math.max(0, Math.floor((Date.now() - new Date(timestamp).getTime()) / 1000))
	if (seconds < 60) return `${seconds}s ago`
	if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
	if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
	return `${Math.floor(seconds / 86400)}d ago`
}

function relativeTimeFuture(timestamp: string) {
	const seconds = Math.max(0, Math.floor((new Date(timestamp).getTime() - Date.now()) / 1000))
	if (seconds < 3600) return `in ${Math.ceil(seconds / 60)}m`
	if (seconds < 86400) return `in ${Math.ceil(seconds / 3600)}h`
	return `in ${Math.ceil(seconds / 86400)}d`
}

function formatBytes(bytes: number) {
	if (bytes < 1024) return `${bytes} B`
	if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`
	if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`
	return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} GiB`
}

function capitalize(value: string) {
	return value.charAt(0).toUpperCase() + value.slice(1)
}
