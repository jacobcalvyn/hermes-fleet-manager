import type { ChatEvent, FleetStateEvent, Operation, OperationPage, Overview } from './types'

export class ApiError extends Error {
  status: number
  stage: string
  action: string
  retryable: boolean

  constructor(status: number, message: string, details: { stage?: string; action?: string; retryable?: boolean } = {}) {
    super(message)
    this.status = status
    this.stage = details.stage ?? ''
    this.action = details.action ?? ''
    this.retryable = details.retryable === true
  }
}

export async function apiRequest<T>(token: string, path: string, options: RequestInit = {}): Promise<T> {
  const response = await fetch(path, {
    ...options,
    headers: {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      ...options.headers,
    },
  })
  if (!response.ok) {
    let message = `Request failed with status ${response.status}`
    let details: { stage?: string; action?: string; retryable?: boolean } = {}
    try {
      const payload = await response.json() as Record<string, unknown>
      if (typeof payload.error === 'string' && payload.error) message = payload.error
      details = {
        ...(typeof payload.stage === 'string' ? { stage: payload.stage } : {}),
        ...(typeof payload.action === 'string' ? { action: payload.action } : {}),
        ...(typeof payload.retryable === 'boolean' ? { retryable: payload.retryable } : {}),
      }
    } catch {
      // Keep the HTTP status message when the server did not return JSON.
    }
    throw new ApiError(response.status, message, details)
  }
  if (response.status === 204) return undefined as T
  return response.json() as Promise<T>
}

export function getOverview(token: string, options: RequestInit = {}) {
  return apiRequest<Overview>(token, '/api/v1/overview', options)
}

export async function streamFleetEvents(
	token: string,
	onEvent: (event: FleetStateEvent) => void,
	signal: AbortSignal,
) {
	const response = await fetch('/api/v1/events', {
		headers: { Authorization: `Bearer ${token}`, Accept: 'text/event-stream' },
		cache: 'no-store',
		signal,
	})
	if (!response.ok || !response.body) {
		throw new ApiError(response.status, `State stream failed with status ${response.status}`)
	}
	const reader = response.body.getReader()
	const decoder = new TextDecoder()
	let buffer = ''
	try {
		while (true) {
			const { done, value } = await reader.read()
			if (done) return
			buffer += decoder.decode(value, { stream: true }).replace(/\r\n/g, '\n')
			let boundary = buffer.indexOf('\n\n')
			while (boundary >= 0) {
				const block = buffer.slice(0, boundary)
				buffer = buffer.slice(boundary + 2)
				const data = block.split('\n')
					.filter((line) => line.startsWith('data:'))
					.map((line) => line.slice(5).trimStart())
					.join('\n')
				if (data) onEvent(JSON.parse(data) as FleetStateEvent)
				boundary = buffer.indexOf('\n\n')
			}
		}
	} finally {
		await reader.cancel().catch(() => undefined)
		reader.releaseLock()
	}
}

export async function streamChatEvents(
	token: string,
	sessionID: string,
	afterID: number,
	onEvent: (event: ChatEvent) => void,
	onCursor: (id: number) => void,
	signal: AbortSignal,
	onOpen?: () => void,
) {
	const params = new URLSearchParams({ after: String(afterID) })
	const response = await fetch(`/api/v1/chats/${encodeURIComponent(sessionID)}/events?${params.toString()}`, {
		headers: { Authorization: `Bearer ${token}`, Accept: 'text/event-stream' },
		cache: 'no-store',
		signal,
	})
	if (!response.ok || !response.body) {
		throw new ApiError(response.status, `Chat stream failed with status ${response.status}`)
	}
	onOpen?.()
	const reader = response.body.getReader()
	const decoder = new TextDecoder()
	let lineBuffer = ''
	let eventID = ''
	let dataLines: string[] = []
	const dispatch = () => {
		if (dataLines.length === 0) {
			eventID = ''
			return
		}
		const event = JSON.parse(dataLines.join('\n')) as ChatEvent
		const id = eventID ? Number(eventID) : event.id
		if (Number.isSafeInteger(id) && id >= 0) onCursor(id)
		onEvent(event)
		eventID = ''
		dataLines = []
	}
	const consumeLine = (rawLine: string) => {
		const line = rawLine.endsWith('\r') ? rawLine.slice(0, -1) : rawLine
		if (line === '') {
			dispatch()
			return
		}
		if (line.startsWith(':')) return
		const separator = line.indexOf(':')
		const field = separator < 0 ? line : line.slice(0, separator)
		let value = separator < 0 ? '' : line.slice(separator + 1)
		if (value.startsWith(' ')) value = value.slice(1)
		if (field === 'id') eventID = value
		if (field === 'data') dataLines.push(value)
	}
	try {
		while (true) {
			const { done, value } = await reader.read()
			if (done) break
			lineBuffer += decoder.decode(value, { stream: true })
			let newline = lineBuffer.indexOf('\n')
			while (newline >= 0) {
				consumeLine(lineBuffer.slice(0, newline))
				lineBuffer = lineBuffer.slice(newline + 1)
				newline = lineBuffer.indexOf('\n')
			}
		}
		lineBuffer += decoder.decode()
		if (lineBuffer) consumeLine(lineBuffer)
		dispatch()
	} finally {
		await reader.cancel().catch(() => undefined)
		reader.releaseLock()
	}
}

export type NormalizedOperationPage = {
  items: Operation[]
  nextCursor: string | null
  legacy: boolean
}

export async function getOperationsPage(
  token: string,
  cursor: string | null = null,
  options: RequestInit = {},
): Promise<NormalizedOperationPage> {
  const params = new URLSearchParams({ limit: '50' })
  if (cursor) params.set('cursor', cursor)
  const payload = await apiRequest<OperationPage | Operation[] | null>(token, `/api/v1/operations?${params.toString()}`, {
    cache: 'no-store',
    ...options,
  })
  if (Array.isArray(payload) || payload === null) {
    return { items: payload ?? [], nextCursor: null, legacy: true }
  }
  return {
    items: payload.items ?? [],
    nextCursor: payload.next_cursor ?? null,
    legacy: false,
  }
}

export async function getOperations(token: string, options: RequestInit = {}) {
  return (await getOperationsPage(token, null, options)).items
}

const MAX_BUFFERED_DOWNLOAD_BYTES = 256 * 1024 * 1024

async function readBoundedDownload(response: Response) {
  const declaredLength = response.headers.get('Content-Length')
  const parsedLength = declaredLength === null ? null : Number(declaredLength)
  if (parsedLength !== null && (!Number.isFinite(parsedLength) || parsedLength < 0)) {
    await response.body?.cancel().catch(() => undefined)
    throw new Error('The server returned an invalid download size.')
  }
  if (parsedLength !== null && parsedLength > MAX_BUFFERED_DOWNLOAD_BYTES) {
    await response.body?.cancel().catch(() => undefined)
    throw new Error('This browser cannot stream large downloads to disk. Use the authenticated download API from a streaming client.')
  }
  if (!response.body) {
    if (parsedLength === null) {
      throw new Error('This browser cannot safely save a download whose size is unknown. Use a browser with streaming file save support.')
    }
    const blob = await response.blob()
    if (blob.size > MAX_BUFFERED_DOWNLOAD_BYTES) {
      throw new Error('This browser cannot stream large downloads to disk. Use the authenticated download API from a streaming client.')
    }
    return blob
  }

  const reader = response.body.getReader()
  const chunks: ArrayBuffer[] = []
  let received = 0
  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      received += value.byteLength
      if (received > MAX_BUFFERED_DOWNLOAD_BYTES) {
        await reader.cancel()
        throw new Error('This browser cannot stream large downloads to disk. Use the authenticated download API from a streaming client.')
      }
      const chunk = new Uint8Array(value.byteLength)
      chunk.set(value)
      chunks.push(chunk.buffer)
    }
  } catch (error) {
    await reader.cancel().catch(() => undefined)
    throw error
  } finally {
    reader.releaseLock()
  }
  return new Blob(chunks, { type: response.headers.get('Content-Type') ?? 'application/octet-stream' })
}

export async function apiDownloadToFile(token: string, path: string, filename: string, options: RequestInit = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { Authorization: `Bearer ${token}`, ...options.headers },
  })
  if (!response.ok) {
    let message = `Request failed with status ${response.status}`
    try {
      const payload = await response.json()
      if (payload.error) message = payload.error
    } catch {
      // Keep the HTTP status message when the server did not return JSON.
    }
    throw new ApiError(response.status, message)
  }
  const pickerWindow = window as Window & {
    showSaveFilePicker?: (options: { suggestedName: string }) => Promise<{ createWritable: () => Promise<WritableStream<Uint8Array>> }>
  }
  if (pickerWindow.showSaveFilePicker && response.body) {
    try {
      const handle = await pickerWindow.showSaveFilePicker({ suggestedName: filename })
      const writable = await handle.createWritable()
      await response.body.pipeTo(writable)
    } catch (error) {
      await response.body.cancel().catch(() => undefined)
      throw error
    }
    return
  }
  const blob = await readBoundedDownload(response)
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  window.setTimeout(() => URL.revokeObjectURL(url), 0)
}
