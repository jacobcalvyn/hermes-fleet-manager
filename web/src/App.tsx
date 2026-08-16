import { FormEvent, type KeyboardEvent as ReactKeyboardEvent, lazy, Suspense, useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import {
  Activity,
  Archive,
  ArrowLeft,
  Bell,
  Bot,
  Boxes,
  Brain,
  Check,
  ChevronRight,
  CircleStop,
  Copy,
  Download,
  ExternalLink,
  Eye,
  EyeOff,
  FileOutput,
  History,
  KeyRound,
  LoaderCircle,
  LogOut,
  Menu,
  MessageCircle,
  PanelLeftClose,
  PanelLeftOpen,
  Plug,
  Play,
  Plus,
  RefreshCw,
  Server,
  Settings,
  ShieldCheck,
  Trash2,
  Wrench,
  X,
  Zap,
} from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import { ApiError, apiDownloadToFile, apiRequest, getOperations, getOperationsPage, getOverview, streamChatEvents, streamFleetEvents } from './api'
import type { OptimisticChatMessage } from './FleetAssistantThread'
import OutputsView from './OutputsView'
import type { Backup, ChatEvent, ChatSession, ChatThread, CodexAuthSession, CredentialReveal, Credentials, FleetStateEvent, HermesProfileInventory, HermesReleaseCatalog, HermesUpdate, Host, Instance, MCPConfiguration, MCPDiscoveredTool, MCPDiscoveryResult, MCPServerConfiguration, MessagingConfiguration, ObservationCheck, Operation, Overview, RecoveryPoint, RemoteAccessConfiguration, RemoteAccessMode, RemoteAccessPublishedRoute, RuntimeHealth, SystemInfo } from './types'

const FleetAssistantThread = lazy(() => import('./FleetAssistantThread'))

type View = 'fleet' | 'hosts' | 'chat' | 'outputs' | 'alerts' | 'operations' | 'system'
type NavigationSection = {
	id: 'primary' | 'fleet' | 'observability' | 'administration'
	label: string
	items: Array<{ id: View; label: string; icon: LucideIcon }>
}
type InstanceTab = 'overview' | 'access' | 'configuration' | 'profiles' | 'messaging' | 'mcp' | 'recovery' | 'diagnostics' | 'operations'
type SystemSection = 'general' | 'runtime-health' | 'remote-access' | 'backups'
type ControlPlaneStatus = 'CHECKING' | 'ONLINE' | 'OFFLINE'
type HermesUpdateFlow = {
	status: 'idle' | 'running' | 'success' | 'error'
	kind: HermesUpdate['update_kind']
	step: number
	targetVersion: string
	detail: string
	resumeAfterUpdate: boolean
}

type OperationGroup = {
	id: string
	operations: Operation[]
	summary: string
	type: string
	status: string
	actor: string
	createdAt: string
	updatedAt: string
}

type FleetAlertRecord = {
	id: string
	state: 'ACTIVE' | 'RECOVERED' | 'FAILED'
	resolution?: 'RECOVERED' | 'SUPERSEDED'
	severity: 'CRITICAL' | 'WARNING'
	title: string
	detail: string
	source: string
	detectedAt: string
	evidence: string[]
	action: { label: string; view: View; instanceID?: string; systemSection?: SystemSection }
}

type CodexFormState = {
	model: string
	reasoning: string
	service_tier: string
}

const focusableSelector = [
	'a[href]',
	'button:not([disabled])',
	'input:not([disabled])',
	'select:not([disabled])',
	'textarea:not([disabled])',
	'[tabindex]:not([tabindex="-1"])',
].join(', ')

const hermesReservedProfileNames = new Set(['hermes', 'root', 'sudo', 'test', 'tmp'])

function codexFormFromInstance(instance: Instance): CodexFormState {
	return {
		model: instance.codex_configured ? instance.model : instance.observation?.recommended_model ?? '',
		reasoning: instance.codex_configured ? instance.reasoning : 'medium',
		service_tier: instance.codex_configured ? instance.service_tier : 'normal',
	}
}

function codexFormsEqual(left: CodexFormState, right: CodexFormState) {
	return left.model === right.model && left.reasoning === right.reasoning && left.service_tier === right.service_tier
}

function mergeOperations(current: Operation[] | null, operation: Operation) {
	const next = (current ?? []).filter((item) => item.id !== operation.id)
	next.push(operation)
	return next.sort((left, right) => new Date(right.created_at).getTime() - new Date(left.created_at).getTime())
}

function mergeOperationLists(primary: Operation[], secondary: Operation[]) {
	const merged = new Map<string, Operation>()
	for (const operation of [...primary, ...secondary]) {
		const current = merged.get(operation.id)
		if (!current || new Date(operation.updated_at).getTime() >= new Date(current.updated_at).getTime()) {
			merged.set(operation.id, operation)
		}
	}
	return [...merged.values()].sort((left, right) => new Date(right.created_at).getTime() - new Date(left.created_at).getTime())
}

function useDialogAccessibility(onClose: () => void, closeEnabled = true) {
	const dialogRef = useRef<HTMLDivElement>(null)
	const restoreFocusRef = useRef<HTMLElement | null>(null)
	const onCloseRef = useRef(onClose)
	const closeEnabledRef = useRef(closeEnabled)

	useEffect(() => {
		onCloseRef.current = onClose
		closeEnabledRef.current = closeEnabled
	}, [closeEnabled, onClose])

	useEffect(() => {
		restoreFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
		const dialog = dialogRef.current
		if (!dialog) return
		const firstFocusable = dialog.querySelector<HTMLElement>(
			'button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
		)
		firstFocusable?.focus()
		return () => restoreFocusRef.current?.focus()
	}, [])

	const onKeyDown = (event: ReactKeyboardEvent<HTMLDivElement>) => {
		if (event.key === 'Escape' && closeEnabledRef.current) {
			event.preventDefault()
			onCloseRef.current()
			return
		}
		if (event.key !== 'Tab') return
		const dialog = dialogRef.current
		if (!dialog) return
		const focusable = Array.from(dialog.querySelectorAll<HTMLElement>(
			'button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
		)).filter((element) => element.getClientRects().length > 0)
		if (focusable.length === 0) {
			event.preventDefault()
			dialog.focus()
			return
		}
		const first = focusable[0]
		const last = focusable[focusable.length - 1]
		if (event.shiftKey && document.activeElement === first) {
			event.preventDefault()
			last.focus()
		} else if (!event.shiftKey && document.activeElement === last) {
			event.preventDefault()
			first.focus()
		}
	}

	return { dialogRef, onKeyDown }
}

async function loadOperationHistory(token: string, signal?: AbortSignal) {
	try {
		return (await getOperations(token, { signal })) ?? []
	} catch (requestError) {
		if (!(requestError instanceof ApiError) || ![404, 405].includes(requestError.status)) throw requestError
		const overview = await getOverview(token, { signal })
		return overview.operations ?? []
	}
}

async function loadOperationByID(token: string, operationID: string, signal?: AbortSignal) {
	try {
		return await apiRequest<Operation>(token, `/api/v1/operations/${operationID}`, {
			cache: 'no-store',
			signal,
		})
	} catch (requestError) {
		if (!(requestError instanceof ApiError) || ![404, 405].includes(requestError.status)) throw requestError
		return (await loadOperationHistory(token, signal)).find((item) => item.id === operationID)
	}
}

async function waitForOperation(
	token: string,
	operationID: string,
	signal: AbortSignal,
	timeout = 20 * 60 * 1000,
) {
	const operation = await waitForOperationResult(token, operationID, signal, undefined, timeout)
	if (operation.status === 'FAILED') throw new Error(operation.error || operation.progress?.detail || `${operation.summary} failed`)
	return operation
}

async function waitForOperationResult(
	token: string,
	operationID: string,
	signal: AbortSignal,
	onProgress?: (operation: Operation) => void,
	timeout = 20 * 60 * 1000,
) {
	const deadline = Date.now() + timeout
	while (Date.now() < deadline) {
		const operation = await loadOperationByID(token, operationID, signal)
		if (operation) onProgress?.(operation)
		if (operation?.status === 'SUCCEEDED' || operation?.status === 'FAILED') return operation
		await sleep(1000, signal)
	}
	throw new Error('The operation did not finish before the timeout')
}

async function waitForRecoveryPoint(
	token: string,
	instanceID: string,
	recoveryPointID: string,
	signal: AbortSignal,
	timeout = 20 * 60 * 1000,
) {
	const deadline = Date.now() + timeout
	while (Date.now() < deadline) {
		const points = await apiRequest<RecoveryPoint[]>(token, `/api/v1/instances/${instanceID}/recovery-points`, { signal })
		const point = (points ?? []).find((item) => item.id === recoveryPointID)
		if (point?.status === 'READY') return point
		if (point?.status === 'FAILED') throw new Error(point.error || 'Backup creation failed')
		await sleep(1000, signal)
	}
	throw new Error('The Host Agent did not finish creating this backup before the timeout')
}

const emptyOverview: Overview = { hosts: [], instances: [], operations: [] }
const hermesUpdateSteps = (flow: HermesUpdateFlow) => flow.kind === 'RUNTIME_REFRESH'
	? [
		'Prepare managed runtime',
		'Stop instance',
		'Create verified backup',
		'Refresh managed runtime',
		flow.resumeAfterUpdate ? 'Restart instance' : 'Keep instance stopped',
		'Verify Hermes',
	]
	: [
		'Prepare release',
		'Stop instance',
		'Create verified backup',
		'Install and verify Hermes',
		flow.resumeAfterUpdate ? 'Restart instance' : 'Keep instance stopped',
		'Confirm installed version',
	]

function persistentHermesUpdateFlow(operation?: Operation): HermesUpdateFlow {
	const idle: HermesUpdateFlow = { status: 'idle', kind: 'NONE', step: 0, targetVersion: '', detail: '', resumeAfterUpdate: false }
	if (!operation || operation.status === 'SUCCEEDED') return idle
	const targetVersion = typeof operation.metadata?.to_version === 'string' ? operation.metadata.to_version : ''
	const kind: HermesUpdate['update_kind'] = operation.metadata?.update_kind === 'RUNTIME_REFRESH' ? 'RUNTIME_REFRESH' : 'VERSION_UPDATE'
	const resumeAfterUpdate = operation.metadata?.original_status === 'RUNNING'
	const refresh = kind === 'RUNTIME_REFRESH'
	const stage = operation.progress?.stage ?? ''
	const stages: Record<string, { step: number; detail: string }> = {
		PREPARING_RELEASE: { step: 0, detail: refresh ? `Preparing the managed runtime for Hermes ${targetVersion}` : `Preparing the verified Hermes ${targetVersion} release on the host` },
		STOPPING: { step: 1, detail: resumeAfterUpdate ? 'Stopping the instance safely' : 'Confirming that the instance remains stopped' },
		BACKING_UP: { step: 2, detail: 'Creating an encrypted rollback backup and verifying it in the control plane' },
		INSTALLING: { step: 3, detail: refresh ? `Refreshing managed runtime components while keeping Hermes ${targetVersion}` : `Installing Hermes ${targetVersion} and running runtime health checks` },
		RESTORING_STATE: { step: 4, detail: resumeAfterUpdate ? `Restarting the ${refresh ? 'refreshed' : 'updated'} instance` : 'Preserving the stopped instance state' },
		VERIFYING_VERSION: { step: 5, detail: refresh ? `Verifying Hermes ${targetVersion} and its managed services` : `Confirming that Hermes ${targetVersion} is installed` },
	}
	const progress = stages[stage] ?? { step: 0, detail: refresh ? `Managed runtime maintenance for Hermes ${targetVersion} is queued on the Host Agent` : `Hermes ${targetVersion} update is queued on the Host Agent` }
	if (operation.status === 'FAILED') {
		return {
			status: 'error',
			kind,
			step: progress.step,
			targetVersion,
			detail: operation.error || (refresh ? `Managed runtime maintenance for Hermes ${targetVersion} failed` : `Hermes ${targetVersion} update failed`),
			resumeAfterUpdate,
		}
	}
	return { status: 'running', kind, step: progress.step, targetVersion, detail: progress.detail, resumeAfterUpdate }
}

const chatSidebarCollapsedStorageKey = 'fleet-chat-sidebar-collapsed'
const navigationStorageKey = 'fleet-navigation-state'
const instanceTabStoragePrefix = 'fleet-instance-tab:'
const selectedChatSessionStorageKey = 'fleet-selected-chat-session'

const validViews: View[] = ['fleet', 'hosts', 'chat', 'outputs', 'alerts', 'operations', 'system']
const validInstanceTabs: InstanceTab[] = ['overview', 'access', 'configuration', 'profiles', 'messaging', 'mcp', 'recovery', 'diagnostics', 'operations']

type StoredNavigation = {
  view: View
  selectedInstanceID: string
}

function readStoredNavigation(): StoredNavigation | null {
  try {
    const stored = JSON.parse(window.localStorage.getItem(navigationStorageKey) ?? 'null') as Partial<StoredNavigation> | null
    if (!stored || !validViews.includes(stored.view as View)) return null
    return {
      view: stored.view as View,
      selectedInstanceID: typeof stored.selectedInstanceID === 'string' ? stored.selectedInstanceID : '',
    }
  } catch {
    return null
  }
}

function readStoredInstanceTab(instanceID: string): InstanceTab {
  try {
    const stored = window.localStorage.getItem(`${instanceTabStoragePrefix}${instanceID}`) as InstanceTab | null
    if (stored && validInstanceTabs.includes(stored)) return stored
  } catch {
    // Fall through to the default tab when browser storage is unavailable.
  }
  return 'overview'
}

function readStoredChatSession(): string {
	try {
		return window.localStorage.getItem(selectedChatSessionStorageKey) ?? ''
	} catch {
		return ''
	}
}

function readStoredChatSidebarCollapsed(): boolean | null {
  try {
    const stored = window.localStorage.getItem(chatSidebarCollapsedStorageKey)
    if (stored === 'true') return true
    if (stored === 'false') return false
  } catch {
    // Fall through to the existing first-load behavior when storage is unavailable.
  }
  return null
}

export default function App() {
  const [token, setToken] = useState(() => sessionStorage.getItem('fleet-admin-token') ?? '')
  const [overview, setOverview] = useState<Overview>(emptyOverview)
  const [view, setView] = useState<View>(() => window.location.hash.startsWith('#system/') ? 'system' : readStoredNavigation()?.view ?? 'chat')
  const [loading, setLoading] = useState(Boolean(token))
  const [error, setError] = useState('')
  const [createOpen, setCreateOpen] = useState(false)
  const [mobileNav, setMobileNav] = useState(false)
  const [mobileNavigationLayout, setMobileNavigationLayout] = useState(() => window.matchMedia('(max-width: 720px)').matches)
  const [selectedInstanceID, setSelectedInstanceID] = useState(() => window.location.hash.startsWith('#system/') ? '' : readStoredNavigation()?.selectedInstanceID ?? '')
	const [requestedChatSessionID, setRequestedChatSessionID] = useState('')
	const [requestedOutputInstanceID, setRequestedOutputInstanceID] = useState('')
	const [chatSidebarCollapsed, setChatSidebarCollapsedState] = useState<boolean | null>(readStoredChatSidebarCollapsed)
  const [refreshSignal, setRefreshSignal] = useState(0)
  const [controlPlaneStatus, setControlPlaneStatus] = useState<ControlPlaneStatus>(token ? 'CHECKING' : 'OFFLINE')
  const [alertHealth, setAlertHealth] = useState<RuntimeHealth | null>(null)
  const [alertSystemInfo, setAlertSystemInfo] = useState<SystemInfo | null>(null)
  const [alertBackups, setAlertBackups] = useState<Backup[]>([])
  const [alertDataError, setAlertDataError] = useState('')
  const [alertSourcesLoaded, setAlertSourcesLoaded] = useState(false)
  const [operationsNextCursor, setOperationsNextCursor] = useState<string | null>(null)
  const [loadingMoreOperations, setLoadingMoreOperations] = useState(false)
  const [refreshingSelectedObservation, setRefreshingSelectedObservation] = useState(false)
	const [fleetRemoteRoutes, setFleetRemoteRoutes] = useState<Record<string, RemoteAccessPublishedRoute>>({})
  const refreshController = useRef<AbortController | null>(null)
  const loadMoreController = useRef<AbortController | null>(null)
  const refreshSequence = useRef(0)
  const operationsNextCursorRef = useRef<string | null>(null)
  const operationHistoryExpanded = useRef(false)
  const mobileNavigationRef = useRef<HTMLElement | null>(null)
  const mobileNavigationTriggerRef = useRef<HTMLButtonElement | null>(null)
	const mobileNavigationCloseRef = useRef<HTMLButtonElement | null>(null)
	const setChatSidebarCollapsed = useCallback((collapsed: boolean) => {
		setChatSidebarCollapsedState(collapsed)
		try {
			window.localStorage.setItem(chatSidebarCollapsedStorageKey, String(collapsed))
		} catch {
			// The in-memory preference still works when browser storage is unavailable.
		}
	}, [])
  const restoreMobileNavigationFocus = useRef(true)
	const stateStreamRef = useRef({ streamID: '', revision: 0 })

  useEffect(() => {
    try {
      window.localStorage.setItem(navigationStorageKey, JSON.stringify({ view, selectedInstanceID }))
    } catch {
      // Navigation still works in memory when browser storage is unavailable.
    }
  }, [selectedInstanceID, view])

  const runRefresh = useCallback(async (showLoading = false) => {
    if (!token) return
    const sequence = refreshSequence.current + 1
    refreshSequence.current = sequence
    refreshController.current?.abort()
    const controller = new AbortController()
    refreshController.current = controller
    if (showLoading) setLoading(true)
    try {
      const [data, operationResult, remoteAccessResult] = await Promise.all([
        getOverview(token, { signal: controller.signal }),
        getOperationsPage(token, null, { signal: controller.signal })
          .then((operationPage) => ({ operationPage, error: null }))
          .catch((requestError: unknown) => ({ operationPage: null, error: requestError })),
		apiRequest<RemoteAccessConfiguration>(token, '/api/v1/system/remote-access/configuration', { cache: 'no-store', signal: controller.signal })
			.then((configuration) => ({ configuration, error: null }))
			.catch((requestError: unknown) => ({ configuration: null, error: requestError })),
      ])
      if (controller.signal.aborted || sequence !== refreshSequence.current) return
      const operationHistory = operationResult.error ? data.operations ?? [] : operationResult.operationPage?.items ?? []
      if (!operationResult.error && !operationHistoryExpanded.current) {
        const nextCursor = operationResult.operationPage?.nextCursor ?? null
        operationsNextCursorRef.current = nextCursor
        setOperationsNextCursor(nextCursor)
      }
      setOverview((current) => ({
        hosts: data.hosts ?? [],
        instances: data.instances ?? [],
        operations: mergeOperationLists(
          operationHistory,
          operationHistoryExpanded.current
            ? current.operations ?? []
            : (current.operations ?? []).filter((operation) => ['PENDING', 'RUNNING'].includes(operation.status)),
        ),
		stream_id: data.stream_id,
		state_revision: data.state_revision,
      }))
		if (!remoteAccessResult.error && remoteAccessResult.configuration) {
			setFleetRemoteRoutes(Object.fromEntries((remoteAccessResult.configuration.instance_routes ?? []).map((route) => [route.instance_id, route])))
		}
		if (data.stream_id) {
			stateStreamRef.current = { streamID: data.stream_id, revision: data.state_revision ?? 0 }
		}
      const historyUnavailable = operationResult.error && !(operationResult.error instanceof ApiError && [404, 405].includes(operationResult.error.status))
      setError(historyUnavailable
        ? operationResult.error instanceof Error ? `Operation history: ${operationResult.error.message}` : 'Operation history could not be loaded'
        : '')
      setControlPlaneStatus('ONLINE')
    } catch (requestError) {
      if (requestError instanceof DOMException && requestError.name === 'AbortError') return
      if (sequence !== refreshSequence.current) return
      setControlPlaneStatus('OFFLINE')
      if (requestError instanceof ApiError && requestError.status === 401) {
        sessionStorage.removeItem('fleet-admin-token')
        setToken('')
      } else {
        setError(requestError instanceof Error ? requestError.message : 'Could not load fleet state')
      }
    } finally {
      if (sequence === refreshSequence.current) {
        if (refreshController.current === controller) refreshController.current = null
        setLoading(false)
      }
    }
  }, [token])

  const refresh = useCallback(() => runRefresh(false), [runRefresh])
  const recordOperation = useCallback((operation: Operation) => {
    if (!operation?.id) return
    setOverview((current) => ({
      ...current,
      operations: mergeOperations(current.operations, operation),
    }))
  }, [])

  useEffect(() => {
    let stopped = false
    let timer = 0
    const poll = async (showLoading = false) => {
      await runRefresh(showLoading)
      if (!stopped) timer = window.setTimeout(() => void poll(false), 30000)
    }
    const initial = window.setTimeout(() => void poll(true), 0)
    return () => {
      stopped = true
      window.clearTimeout(initial)
      window.clearTimeout(timer)
      refreshController.current?.abort()
      loadMoreController.current?.abort()
    }
  }, [runRefresh])

	useEffect(() => {
		if (!token || view !== 'alerts') return
		let stopped = false
		let timer = 0
		let controller = new AbortController()
		const load = async () => {
			controller.abort()
			controller = new AbortController()
			try {
				const [healthResult, infoResult, backupsResult] = await Promise.allSettled([
					apiRequest<RuntimeHealth>(token, '/api/v1/system/runtime-health', { cache: 'no-store', signal: controller.signal }),
					apiRequest<SystemInfo>(token, '/api/v1/system', { cache: 'no-store', signal: controller.signal }),
					apiRequest<Backup[]>(token, '/api/v1/backups', { cache: 'no-store', signal: controller.signal }),
				])
				if (stopped || controller.signal.aborted) return
				const failures: string[] = []
				if (healthResult.status === 'fulfilled') setAlertHealth(healthResult.value)
				else failures.push(`Runtime health: ${requestErrorMessage(healthResult.reason)}`)
				if (infoResult.status === 'fulfilled') setAlertSystemInfo(infoResult.value)
				else failures.push(`System information: ${requestErrorMessage(infoResult.reason)}`)
				if (backupsResult.status === 'fulfilled') setAlertBackups(backupsResult.value ?? [])
				else failures.push(`Control-plane backups: ${requestErrorMessage(backupsResult.reason)}`)
				setAlertDataError(failures.join(' · '))
			} catch (requestError) {
				if (stopped || controller.signal.aborted) return
				setAlertDataError(requestError instanceof Error ? requestError.message : 'Alert sources could not be loaded')
			} finally {
				if (!stopped) {
					setAlertSourcesLoaded(true)
					timer = window.setTimeout(() => void load(), 30000)
				}
			}
		}
		const initial = window.setTimeout(() => {
			setAlertSourcesLoaded(false)
			void load()
		}, 0)
		return () => {
			stopped = true
			window.clearTimeout(initial)
			window.clearTimeout(timer)
			controller.abort()
		}
	}, [refreshSignal, token, view])

	useEffect(() => {
		if (!token) return
		let stopped = false
		let reconnectTimer = 0
		let refreshTimer = 0
		let retryDelay = 1000
		let controller = new AbortController()
		const receive = (event: FleetStateEvent) => {
			const current = stateStreamRef.current
			if (event.stream_id === current.streamID && event.revision <= current.revision) return
			stateStreamRef.current = { streamID: event.stream_id, revision: event.revision }
			window.clearTimeout(refreshTimer)
			refreshTimer = window.setTimeout(() => void runRefresh(false), 100)
		}
		const connect = async () => {
			controller = new AbortController()
			try {
				await streamFleetEvents(token, receive, controller.signal)
				retryDelay = 1000
			} catch (streamError) {
				if (controller.signal.aborted || stopped) return
				if (streamError instanceof ApiError && streamError.status === 401) {
					sessionStorage.removeItem('fleet-admin-token')
					setToken('')
					return
				}
			} finally {
				if (!controller.signal.aborted && !stopped) {
					reconnectTimer = window.setTimeout(() => void connect(), retryDelay)
					retryDelay = Math.min(retryDelay * 2, 30000)
				}
			}
		}
		void connect()
		return () => {
			stopped = true
			controller.abort()
			window.clearTimeout(reconnectTimer)
			window.clearTimeout(refreshTimer)
		}
	}, [runRefresh, token])

  const loadMoreOperations = async () => {
    const cursor = operationsNextCursorRef.current
    if (!cursor || loadingMoreOperations) return
    loadMoreController.current?.abort()
    const controller = new AbortController()
    loadMoreController.current = controller
    setLoadingMoreOperations(true)
    try {
      const operationPage = await getOperationsPage(token, cursor, { signal: controller.signal })
      if (controller.signal.aborted) return
      setOverview((current) => ({
        ...current,
        operations: mergeOperationLists(current.operations ?? [], operationPage.items),
      }))
      operationHistoryExpanded.current = true
      operationsNextCursorRef.current = operationPage.nextCursor
      setOperationsNextCursor(operationPage.nextCursor)
    } catch (requestError) {
      if (requestError instanceof DOMException && requestError.name === 'AbortError') return
      setError(requestError instanceof Error ? requestError.message : 'Could not load older operations')
    } finally {
      if (loadMoreController.current === controller) loadMoreController.current = null
      if (!controller.signal.aborted) setLoadingMoreOperations(false)
    }
  }

  const refreshActiveView = async () => {
    const selected = (overview.instances ?? []).find((instance) => instance.id === selectedInstanceID)
    setRefreshingSelectedObservation(Boolean(selected))
    setError('')
    try {
      if (selected && ['RUNNING', 'STOPPED', 'FAILED'].includes(selected.status) && !selected.observation_request) {
        await apiRequest(token, `/api/v1/instances/${selected.id}/observations/refresh`, {
          method: 'POST',
          body: '{}',
        })
      }
      setRefreshSignal((current) => current + 1)
      await runRefresh(true)
    } catch (requestError) {
      setError(requestError instanceof Error ? requestError.message : 'Current runtime verification could not be requested')
      await runRefresh(true)
    } finally {
      setRefreshingSelectedObservation(false)
    }
  }

  const closeMobileNavigation = useCallback((restoreFocus = true) => {
    restoreMobileNavigationFocus.current = restoreFocus
    setMobileNav(false)
  }, [])

  useEffect(() => {
    const media = window.matchMedia('(max-width: 720px)')
    const syncLayout = () => setMobileNavigationLayout(media.matches)
    syncLayout()
    media.addEventListener('change', syncLayout)
    return () => media.removeEventListener('change', syncLayout)
  }, [])

  useEffect(() => {
    if (!mobileNav) return
    restoreMobileNavigationFocus.current = true
    const navigation = mobileNavigationRef.current
    const trigger = mobileNavigationTriggerRef.current
    const main = navigation?.nextElementSibling as HTMLElement | null
    main?.setAttribute('inert', '')
    const focusTimer = window.setTimeout(() => mobileNavigationCloseRef.current?.focus(), 0)
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        closeMobileNavigation(true)
        return
      }
      if (event.key !== 'Tab' || !navigation) return
      const focusable = Array.from(navigation.querySelectorAll<HTMLElement>(focusableSelector))
        .filter((element) => !element.hasAttribute('disabled') && element.tabIndex !== -1)
      if (focusable.length === 0) return
      const first = focusable[0]
      const last = focusable[focusable.length - 1]
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault()
        last.focus()
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', onKeyDown)
    return () => {
      window.clearTimeout(focusTimer)
      document.removeEventListener('keydown', onKeyDown)
      main?.removeAttribute('inert')
      if (restoreMobileNavigationFocus.current) trigger?.focus()
    }
  }, [closeMobileNavigation, mobileNav])

  if (!token) {
    return <Login onLogin={(value) => {
      sessionStorage.setItem('fleet-admin-token', value)
      operationHistoryExpanded.current = false
      operationsNextCursorRef.current = null
      setOperationsNextCursor(null)
      setLoading(true)
      setControlPlaneStatus('CHECKING')
      setToken(value)
    }} />
  }

  const hosts = overview.hosts ?? []
  const instances = overview.instances ?? []
  const operations = overview.operations ?? []
	const activeOperationCount = groupOperations(operations).filter((group) => ['PENDING', 'RUNNING'].includes(group.status)).length
  const alertRecords = buildFleetAlertRecords(hosts, instances, operations, alertHealth, alertSystemInfo, alertBackups)
	const activeAlertCount = alertRecords.filter((record) => record.state === 'ACTIVE').length
  const onlineHosts = hosts.filter((host) => host.status === 'ONLINE').length
  const runningInstances = instances.filter((instance) => instance.status === 'RUNNING').length
	const latestPublicationOperations = new Map<string, Operation>()
	for (const operation of operations) {
		if (operation.type !== 'PUBLISH_DASHBOARD' || !operation.instance_id) continue
		const current = latestPublicationOperations.get(operation.instance_id)
		if (!current || Date.parse(operation.created_at) > Date.parse(current.created_at)) latestPublicationOperations.set(operation.instance_id, operation)
	}
	const publicationAttentionInstanceIDs = new Set(instances.filter((instance) => {
		const route = fleetRemoteRoutes[instance.id]
		const publicationFailed = route?.provider_state === 'failed' || route?.provider_state === 'configuration_mismatch' ||
			['failed', 'conflict'].includes(route?.dns_state ?? '') || ['failed', 'conflict'].includes(route?.route_state ?? '') ||
			['unavailable', 'access_protected'].includes(route?.endpoint_state ?? '')
		const latestPublicationFailed = latestPublicationOperations.get(instance.id)?.status === 'FAILED' && !route?.published
		return Boolean(instance.public_hostname && (publicationFailed || latestPublicationFailed))
	}).map((instance) => instance.id))
	const attentionInstances = instances.filter((instance) =>
		instance.status === 'FAILED' || (instance.status === 'DELETING' && Boolean(instance.last_error)) ||
		['DEGRADED', 'MISSING', 'INCOMPLETE'].includes(instanceReadinessStatus(instance)) || publicationAttentionInstanceIDs.has(instance.id),
	).length
  const selectedInstance = instances.find((instance) => instance.id === selectedInstanceID)
  const selectedObservationPending = refreshingSelectedObservation || Boolean(selectedInstance?.observation_request)

  const navigationSections: NavigationSection[] = [
    {
      id: 'primary',
      label: '',
      items: [
		{ id: 'chat', label: 'Chat', icon: MessageCircle },
	  ],
    },
    {
      id: 'fleet',
      label: 'Fleet',
      items: [
        { id: 'fleet', label: 'Instances', icon: Boxes },
        { id: 'hosts', label: 'Hosts', icon: Server },
      ],
    },
    {
      id: 'observability',
      label: 'Observability',
      items: [
		{ id: 'outputs', label: 'Outputs', icon: FileOutput },
        { id: 'alerts', label: 'Alerts', icon: Bell },
        { id: 'operations', label: 'Operations', icon: History },
      ],
    },
    {
      id: 'administration',
      label: 'Administration',
      items: [{ id: 'system', label: 'System', icon: Settings }],
    },
  ]
  const navigation = navigationSections.flatMap((section) => section.items)

	const navigateFromAlert = (target: FleetAlertRecord['action']) => {
		setSelectedInstanceID(target.instanceID ?? '')
		setView(target.view)
		if (target.view === 'system') {
			window.history.pushState(null, '', `#system/${target.systemSection ?? 'runtime-health'}`)
		} else if (window.location.hash.startsWith('#system/')) {
			window.history.replaceState(null, '', window.location.pathname + window.location.search)
		}
	}

  return (
    <div className="app-shell">
      <aside
        ref={mobileNavigationRef}
        id="primary-navigation"
        className={`sidebar ${mobileNav ? 'sidebar-open' : ''}`}
        aria-label="Primary navigation"
        role={mobileNav ? 'dialog' : undefined}
        aria-modal={mobileNav || undefined}
        aria-hidden={mobileNavigationLayout && !mobileNav ? true : undefined}
        inert={mobileNavigationLayout && !mobileNav ? true : undefined}
      >
        <div className="brand">
          <div className="brand-mark"><Boxes size={19} /></div>
          <div><strong>Hermes Fleet</strong><span>Local control plane</span></div>
          <button ref={mobileNavigationCloseRef} className="icon-button mobile-only" onClick={() => closeMobileNavigation(true)} title="Close navigation" aria-label="Close navigation"><X size={18} /></button>
        </div>
        <nav>
          {navigationSections.map((section) => (
            <div key={section.id} className={`nav-section nav-section-${section.id}`} role="group" aria-label={section.label || 'Primary'}>
              {section.label && <span className="nav-section-label" aria-hidden="true">{section.label}</span>}
              {section.items.map((item) => (
                <button key={item.id} className={view === item.id && !selectedInstance ? 'nav-active' : ''} onClick={() => {
							setSelectedInstanceID('')
							if (item.id === 'outputs') setRequestedOutputInstanceID('')
							setView(item.id)
							closeMobileNavigation(true)
							if (item.id === 'system' && !window.location.hash.startsWith('#system/')) window.history.pushState(null, '', '#system/general')
							if (item.id !== 'system' && window.location.hash.startsWith('#system/')) window.history.replaceState(null, '', window.location.pathname + window.location.search)
						}}>
				  <item.icon size={18} /><span>{item.label}</span>{item.id === 'alerts' && activeAlertCount > 0 && <span className="nav-count alert-nav-count" aria-hidden="true">{activeAlertCount}</span>}{item.id === 'operations' && activeOperationCount > 0 && <span className="nav-count" aria-hidden="true" title={`${activeOperationCount} active ${plural(activeOperationCount, 'operation')} · clears automatically when finished`}>{activeOperationCount}</span>}
                </button>
              ))}
            </div>
          ))}
        </nav>
        <div className="sidebar-footer">
          <div className="control-status" role="status">
            <span className={`status-dot ${controlPlaneStatus === 'ONLINE' ? 'online' : controlPlaneStatus === 'OFFLINE' ? 'offline' : ''}`} />
            {controlPlaneStatus === 'ONLINE' ? 'Control plane online' : controlPlaneStatus === 'OFFLINE' ? 'Control plane unavailable' : 'Checking control plane'}
          </div>
          <button onClick={() => { sessionStorage.removeItem('fleet-admin-token'); setToken('') }}><LogOut size={17} />Sign out</button>
        </div>
      </aside>

      <main>
        <header className="topbar">
          <button ref={mobileNavigationTriggerRef} className="icon-button mobile-only" onClick={() => setMobileNav(true)} title="Open navigation" aria-label="Open navigation" aria-haspopup="dialog" aria-expanded={mobileNav} aria-controls="primary-navigation"><Menu size={19} /></button>
          {selectedInstance && <button className="icon-button" onClick={() => setSelectedInstanceID('')} title="Back to instances"><ArrowLeft size={18} /></button>}
          <div>
            <h1>{selectedInstance?.name ?? navigation.find((item) => item.id === view)?.label}</h1>
            <p>{selectedInstance ? instanceHeaderSubtitle(selectedInstance) : viewSubtitle(view)}</p>
          </div>
          <div className="topbar-actions">
            <button
              className="icon-button"
              onClick={() => void refreshActiveView()}
			  title={selectedInstance ? selectedObservationPending ? 'Status refresh pending' : 'Refresh instance status' : 'Reload Fleet data'}
			  aria-label={selectedInstance ? selectedObservationPending ? 'Status refresh pending' : 'Refresh instance status' : 'Reload Fleet data'}
              disabled={loading || selectedObservationPending}
            ><RefreshCw size={18} className={loading || selectedObservationPending ? 'spin' : ''} /></button>
			{selectedInstance?.status === 'RUNNING' && <a className="primary-button dashboard-button" href={selectedInstance.public_dashboard_url || `http://127.0.0.1:${selectedInstance.dashboard_port}`} target="_blank" rel="noreferrer" title={selectedInstance.public_dashboard_url ? 'Open public dashboard' : 'Open host-local dashboard; remote operators need an SSH tunnel to this port'}><ExternalLink size={17} /><span>{selectedInstance.public_dashboard_url ? 'Open dashboard' : 'Open local dashboard'}</span></a>}
            {view === 'fleet' && !selectedInstance && <button className="primary-button" onClick={() => setCreateOpen(true)} disabled={onlineHosts === 0}><Plus size={18} />Create instance</button>}
          </div>
        </header>

        {error && <div className="error-banner"><span>{error}</span><button className="icon-button" onClick={() => setError('')} title="Dismiss error"><X size={17} /></button></div>}

        <div className="content">
          {selectedInstance ? <InstanceProfile key={selectedInstance.id} instance={selectedInstance} operations={operations.filter((operation) => operation.instance_id === selectedInstance.id)} token={token} onChanged={refresh} onOperation={recordOperation} refreshSignal={refreshSignal} /> : view === 'fleet' && (
            <FleetView
              hosts={hosts}
              instances={instances}
              operations={operations}
              counts={{ onlineHosts, runningInstances, attentionInstances }}
			  remoteRoutes={fleetRemoteRoutes}
			  publicationAttentionInstanceIDs={publicationAttentionInstanceIDs}
              token={token}
              onChanged={refresh}
              onOperation={recordOperation}
              loading={loading}
              hasMoreOperations={Boolean(operationsNextCursor)}
              onSelect={setSelectedInstanceID}
            />
          )}
          {!selectedInstance && view === 'hosts' && <HostsView hosts={hosts} instances={instances} operations={operations} token={token} refreshSignal={refreshSignal} onSelectInstance={setSelectedInstanceID} />}
		  {!selectedInstance && view === 'chat' && <ChatView token={token} instances={instances} refreshSignal={refreshSignal} onOperation={recordOperation} initialSessionID={requestedChatSessionID} onInitialSessionHandled={() => setRequestedChatSessionID('')} sidebarCollapsedState={chatSidebarCollapsed} onSidebarCollapsedChange={setChatSidebarCollapsed} onOpenOutputs={(instanceID) => { setRequestedOutputInstanceID(instanceID); setSelectedInstanceID(''); setView('outputs') }} />}
		  {!selectedInstance && view === 'outputs' && <OutputsView token={token} instances={instances} refreshSignal={refreshSignal} initialInstanceID={requestedOutputInstanceID} onOpenChat={(sessionID) => { setRequestedChatSessionID(sessionID); setSelectedInstanceID(''); setView('chat') }} />}
          {!selectedInstance && view === 'alerts' && <AlertsView records={alertRecords} loading={!alertSourcesLoaded} error={alertDataError} onNavigate={navigateFromAlert} />}
		  {!selectedInstance && view === 'operations' && <OperationsView operations={operations} instances={instances} token={token} nextCursor={operationsNextCursor} loadingMore={loadingMoreOperations} onLoadMore={loadMoreOperations} onChanged={refresh} />}
          {!selectedInstance && view === 'system' && <SystemView token={token} refreshSignal={refreshSignal} />}
        </div>
      </main>

      {createOpen && <CreateInstanceDialog hosts={hosts} token={token} onClose={() => setCreateOpen(false)} onCreated={() => { setCreateOpen(false); void refresh() }} onOperation={recordOperation} />}
    </div>
  )
}

function ChatView({ token, instances, refreshSignal, onOperation, initialSessionID = '', onInitialSessionHandled, sidebarCollapsedState, onSidebarCollapsedChange, onOpenOutputs }: {
	token: string
	instances: Instance[]
	refreshSignal: number
	onOperation: (operation: Operation) => void
	initialSessionID?: string
	onInitialSessionHandled?: () => void
	sidebarCollapsedState: boolean | null
	onSidebarCollapsedChange: (collapsed: boolean) => void
	onOpenOutputs: (instanceID: string) => void
}) {
	const availableInstances = instances.filter((instance) => instance.status === 'RUNNING' && instance.managed_path)
	const [sessions, setSessions] = useState<ChatSession[]>([])
	const [seenAssistantMessages, setSeenAssistantMessages] = useState<Record<string, string>>({})
	const [selectedID, setSelectedID] = useState(readStoredChatSession)
	const sidebarCollapsed = sidebarCollapsedState ?? false
	const [thread, setThread] = useState<ChatThread | null>(null)
	const [optimisticMessage, setOptimisticMessage] = useState<OptimisticChatMessage | null>(null)
	const [loading, setLoading] = useState(true)
	const [creating, setCreating] = useState(false)
	const [creatingInstanceID, setCreatingInstanceID] = useState('')
	const [createDialog, setCreateDialog] = useState({ open: false, preserveCollapsed: false, top: 0, left: 0 })
	const [createError, setCreateError] = useState('')
	const [sending, setSending] = useState(false)
	const [canceling, setCanceling] = useState(false)
	const [editingSessionConfiguration, setEditingSessionConfiguration] = useState(false)
	const [savingSessionConfiguration, setSavingSessionConfiguration] = useState(false)
	const [copiedSessionID, setCopiedSessionID] = useState('')
	const [sessionConfiguration, setSessionConfiguration] = useState<CodexFormState>({ model: '', reasoning: '', service_tier: '' })
	const [deleteConfirmationID, setDeleteConfirmationID] = useState('')
	const [deletingID, setDeletingID] = useState('')
	const [streamState, setStreamState] = useState<'CONNECTING' | 'LIVE' | 'RECONNECTING'>('CONNECTING')
	const [liveResponse, setLiveResponse] = useState<{ operationID: string; content: string } | null>(null)
	const [error, setError] = useState('')
	const threadLoadSequence = useRef(0)
	const chatCursor = useRef({ sessionID: '', delivered: 0 })
	const seenSessionsInitialized = useRef(false)
	const sidebarContentID = useId()

	useEffect(() => {
		try {
			if (selectedID) window.localStorage.setItem(selectedChatSessionStorageKey, selectedID)
			else window.localStorage.removeItem(selectedChatSessionStorageKey)
		} catch {
			// Keep the selected chat in memory when browser storage is unavailable.
		}
	}, [selectedID])

	const copySessionID = async () => {
		if (!thread) return
		try {
			await navigator.clipboard.writeText(thread.session.id)
			setCopiedSessionID(thread.session.id)
			window.setTimeout(() => setCopiedSessionID((current) => current === thread.session.id ? '' : current), 1600)
		} catch {
			setError('Session ID could not be copied')
		}
	}

	const loadSessions = useCallback(async (preserveSelection = true) => {
		const receivedItems = await apiRequest<ChatSession[]>(token, '/api/v1/chats', { cache: 'no-store' })
		const items = (receivedItems ?? []).map(normalizeChatSessionTitle)
		const ordered = [...(items ?? [])].sort((left, right) =>
			new Date(right.updated_at).getTime() - new Date(left.updated_at).getTime() || right.id.localeCompare(left.id))
		setSessions(ordered)
		if (!seenSessionsInitialized.current) {
			seenSessionsInitialized.current = true
			setSeenAssistantMessages(Object.fromEntries(ordered
				.filter((session) => session.last_message_role === 'assistant' && session.last_message_id)
				.map((session) => [session.id, session.last_message_id as string])))
		}
		setSelectedID((current) => preserveSelection && current && ordered.some((item) => item.id === current) ? current : ordered[0]?.id ?? '')
		return ordered
	}, [setSeenAssistantMessages, setSelectedID, setSessions, token])

	const loadThread = useCallback(async (sessionID: string) => {
		const sequence = ++threadLoadSequence.current
		if (!sessionID) {
			setThread(null)
			setLiveResponse(null)
			return null
		}
		const receivedValue = await apiRequest<ChatThread>(token, `/api/v1/chats/${sessionID}`, { cache: 'no-store' })
		const value = { ...receivedValue, session: normalizeChatSessionTitle(receivedValue.session) }
		const cursor = chatCursor.current
		if (sequence === threadLoadSequence.current &&
			(cursor.sessionID !== sessionID || value.last_cursor >= cursor.delivered)) {
			const latestAssistant = [...value.messages].reverse().find((message) => message.role === 'assistant')
			if (latestAssistant) {
				setSeenAssistantMessages((current) => current[sessionID] === latestAssistant.id
					? current
					: { ...current, [sessionID]: latestAssistant.id })
			}
			chatCursor.current = { sessionID, delivered: value.last_cursor }
			setEditingSessionConfiguration(false)
			setThread(value)
			setLiveResponse(value.active_response
				? { operationID: value.active_response.operation_id, content: value.active_response.content ?? '' }
				: null)
		}
		return value
	}, [setEditingSessionConfiguration, setLiveResponse, setSeenAssistantMessages, setThread, token])

	useEffect(() => {
		let stopped = false
		const initial = window.setTimeout(() => {
			void loadSessions().catch((requestError) => {
				if (!stopped) setError(requestError instanceof Error ? requestError.message : 'Chat sessions could not be loaded')
			}).finally(() => { if (!stopped) setLoading(false) })
		}, 0)
		return () => { stopped = true; window.clearTimeout(initial) }
	}, [loadSessions, refreshSignal])

	useEffect(() => {
		const requestedSession = sessions.find((session) => session.id === initialSessionID)
		if (!initialSessionID || !requestedSession) return
		const select = window.setTimeout(() => {
			setSelectedID(initialSessionID)
			onInitialSessionHandled?.()
		}, 0)
		return () => window.clearTimeout(select)
	}, [initialSessionID, onInitialSessionHandled, sessions])

	useEffect(() => {
		if (!thread || thread.session.id !== selectedID || sidebarCollapsedState !== null) return
		onSidebarCollapsedChange(thread.messages.length > 0 || Boolean(thread.active_response))
	}, [onSidebarCollapsedChange, selectedID, sidebarCollapsedState, thread])

	useEffect(() => {
		if (!deleteConfirmationID) return
		const cancelInlineDelete = (event: KeyboardEvent) => {
			if (event.key === 'Escape' && !deletingID) {
				setDeleteConfirmationID('')
			}
		}
		window.addEventListener('keydown', cancelInlineDelete)
		return () => window.removeEventListener('keydown', cancelInlineDelete)
	}, [deleteConfirmationID, deletingID])

	useEffect(() => {
		if (!selectedID) {
			const reset = window.setTimeout(() => {
				setThread(null)
				setLiveResponse(null)
				setOptimisticMessage(null)
			}, 0)
			return () => window.clearTimeout(reset)
		}
		let stopped = false
		let initial = 0
		let afterID = 0
		let retryDelay = 500
		let activeController: AbortController | null = null
		let animationFrame = 0
		let bufferedOperationID = ''
		let bufferedDelta = ''
		chatCursor.current = { sessionID: selectedID, delivered: 0 }
		const flushDelta = () => {
			animationFrame = 0
			if (!bufferedOperationID || !bufferedDelta) return
			const operationID = bufferedOperationID
			const content = bufferedDelta
			bufferedOperationID = ''
			bufferedDelta = ''
			setLiveResponse((current) => current?.operationID === operationID
				? { ...current, content: current.content + content }
				: { operationID, content })
		}
		const scheduleDelta = (event: ChatEvent) => {
			if (bufferedOperationID && bufferedOperationID !== event.operation_id) flushDelta()
			bufferedOperationID = event.operation_id
			bufferedDelta += event.content ?? ''
			if (!animationFrame) animationFrame = window.requestAnimationFrame(flushDelta)
		}
		const receive = (event: ChatEvent) => {
			if (event.type === 'ASSISTANT_DELTA') {
				scheduleDelta(event)
				return
			}
			if (event.type === 'ASSISTANT_ACTIVITY' || event.type === 'ASSISTANT_ARTIFACT') {
				setThread((current) => current && current.session.id === selectedID && !current.events?.some((item) => item.id === event.id)
					? { ...current, events: [...(current.events ?? []), event] }
					: current)
				return
			}
			if (!['RUN_COMPLETED', 'RUN_FAILED', 'RUN_CANCELED'].includes(event.type)) return
			flushDelta()
			// Keep the streaming bubble mounted until the atomic snapshot contains
			// the persisted final message. React then swaps both states together.
			void loadThread(selectedID).then((snapshot) => {
				if (stopped || !snapshot) return
				afterID = Math.max(afterID, snapshot.last_cursor)
				return loadSessions()
			}).catch(() => undefined)
		}
		const connect = async () => {
			while (!stopped) {
				activeController = new AbortController()
				try {
					const snapshot = await loadThread(selectedID)
					if (stopped || activeController.signal.aborted || !snapshot) return
					afterID = Math.max(afterID, snapshot.last_cursor)
					await streamChatEvents(token, selectedID, afterID, receive, (id) => {
						afterID = Math.max(afterID, id)
						if (chatCursor.current.sessionID === selectedID) {
							chatCursor.current.delivered = Math.max(chatCursor.current.delivered, id)
						}
					},
						activeController.signal, () => { retryDelay = 500; setStreamState('LIVE') })
					if (!stopped) setStreamState('RECONNECTING')
				} catch (streamError) {
					if (stopped || activeController.signal.aborted) return
					setStreamState('RECONNECTING')
					if (streamError instanceof ApiError && streamError.status === 401) return
				}
				await new Promise<void>((resolve) => window.setTimeout(resolve, retryDelay))
				retryDelay = Math.min(retryDelay * 2, 10000)
			}
		}
		initial = window.setTimeout(() => {
			setThread(null)
			setLiveResponse(null)
			setOptimisticMessage(null)
			setStreamState('CONNECTING')
			void connect()
		}, 0)
		return () => {
			stopped = true
			window.clearTimeout(initial)
			if (animationFrame) window.cancelAnimationFrame(animationFrame)
			activeController?.abort()
		}
	}, [loadSessions, loadThread, selectedID, token])

	const createSession = async (selectedTargetID: string, preserveCollapsed = false) => {
		if (!selectedTargetID || creating) return
		const keepSidebarCollapsed = preserveCollapsed && sidebarCollapsed
		setCreating(true)
		setCreatingInstanceID(selectedTargetID)
		setCreateError('')
		try {
			const session = await apiRequest<ChatSession>(token, '/api/v1/chats', {
				method: 'POST', body: JSON.stringify({ instance_id: selectedTargetID, title: '' }),
			})
			await loadSessions(false)
			onSidebarCollapsedChange(keepSidebarCollapsed)
			setSelectedID(session.id)
			setCreateDialog({ open: false, preserveCollapsed: false, top: 0, left: 0 })
		} catch (requestError) {
			setCreateError(requestError instanceof Error ? requestError.message : 'Chat session could not be created')
		} finally {
			setCreating(false)
			setCreatingInstanceID('')
		}
	}

	const sendMessage = async (submittedContent: string) => {
		const content = submittedContent.trim()
		if (!selectedID || !content || sending || thread?.messages.some((message) => message.status === 'PENDING')) return
		if ((thread?.messages.length ?? 0) === 0) {
			onSidebarCollapsedChange(true)
		}
		const optimisticID = `optimistic-${selectedID}-${Date.now()}`
		setOptimisticMessage({ id: optimisticID, content, createdAt: new Date().toISOString() })
		setSending(true)
		setError('')
		try {
			const operation = await apiRequest<Operation>(token, `/api/v1/chats/${selectedID}/messages`, {
				method: 'POST', body: JSON.stringify({ content }),
			})
			onOperation(operation)
			setOptimisticMessage((current) => current?.id === optimisticID ? { ...current, operationID: operation.id } : current)
			await Promise.all([loadThread(selectedID), loadSessions()])
		} catch (requestError) {
			setOptimisticMessage((current) => current?.id === optimisticID ? null : current)
			setError(requestError instanceof Error ? requestError.message : 'Message could not be sent')
		} finally {
			setSending(false)
		}
	}

	const cancelResponse = async () => {
		if (!selectedID || canceling) return
		setCanceling(true)
		setError('')
		try {
			await apiRequest<void>(token, `/api/v1/chats/${selectedID}/cancel`, { method: 'POST', body: '{}' })
			await Promise.all([loadThread(selectedID), loadSessions()])
		} catch (requestError) {
			setError(requestError instanceof Error ? requestError.message : 'Response could not be canceled')
		} finally {
			setCanceling(false)
		}
	}

	const deleteSession = async (session: ChatSession) => {
		if (deletingID) return
		setDeletingID(session.id)
		setError('')
		try {
			await apiRequest<void>(token, `/api/v1/chats/${session.id}`, { method: 'DELETE' })
			setDeleteConfirmationID('')
			if (selectedID === session.id) {
				setThread(null)
				setLiveResponse(null)
				setOptimisticMessage(null)
			}
			await loadSessions(true)
		} catch (requestError) {
			setError(requestError instanceof Error ? requestError.message : 'Chat session could not be deleted')
		} finally {
			setDeletingID('')
		}
	}

	const pending = thread?.messages.some((message) => message.status === 'PENDING') ?? false
	const responseInProgress = sending || pending || Boolean(thread?.active_response) || Boolean(liveResponse)
	const threadInstance = thread ? instances.find((instance) => instance.id === thread.session.instance_id) : undefined
	const sessionModelOptions = [...new Set([
		thread?.session.model,
		threadInstance?.model,
		...(threadInstance?.observation?.model_catalog ?? []),
	].filter((model): model is string => Boolean(model)))]
	const sessionConfigurationChanged = thread ? !codexFormsEqual(sessionConfiguration, {
		model: thread.session.model,
		reasoning: thread.session.reasoning,
		service_tier: thread.session.service_tier,
	}) : false
	const saveSessionConfiguration = async (event: FormEvent) => {
		event.preventDefault()
		if (!thread || responseInProgress || savingSessionConfiguration || !sessionConfigurationChanged ||
			!sessionConfiguration.model || !sessionConfiguration.reasoning || !sessionConfiguration.service_tier) return
		setSavingSessionConfiguration(true)
		setError('')
		try {
			const updated = await apiRequest<ChatSession>(token, `/api/v1/chats/${thread.session.id}`, {
				method: 'PATCH', body: JSON.stringify(sessionConfiguration),
			})
			setThread((current) => current?.session.id === updated.id ? { ...current, session: updated } : current)
			setSessions((current) => current.map((session) => session.id === updated.id ? { ...session, ...updated } : session))
			setEditingSessionConfiguration(false)
		} catch (requestError) {
			setError(requestError instanceof Error ? requestError.message : 'Session configuration could not be updated')
		} finally {
			setSavingSessionConfiguration(false)
		}
	}
	const selectSession = (session: ChatSession) => {
		if (!deletingID) setDeleteConfirmationID('')
		if (session.last_message_role === 'assistant' && session.last_message_id) {
			setSeenAssistantMessages((current) => ({ ...current, [session.id]: session.last_message_id as string }))
		}
		setSelectedID(session.id)
	}
	const openCreateDialog = (preserveCollapsed: boolean, anchor: HTMLElement) => {
		const bounds = anchor.getBoundingClientRect()
		const width = 236
		const left = Math.max(8, Math.min(bounds.right + 8, window.innerWidth - width - 8))
		const top = Math.max(8, Math.min(bounds.top, window.innerHeight - 96))
		setCreateError('')
		setCreateDialog({ open: true, preserveCollapsed, top, left })
	}
	const closeCreateDialog = useCallback(() => {
		setCreateDialog({ open: false, preserveCollapsed: false, top: 0, left: 0 })
	}, [])
	const sidebarVisible = !sidebarCollapsed
	return <section className={`section-block first-section chat-shell${sidebarCollapsed ? ' chat-sidebar-hidden' : ''}`}>
		<aside className={`chat-sidebar${sidebarVisible ? '' : ' chat-sidebar-collapsed'}`} aria-label="Chat navigation">
			<div className="chat-sidebar-heading">
				<div className="chat-sidebar-copy"><h2>Chats</h2><p>{sessions.length} {plural(sessions.length, 'session')}</p></div>
				<button className="icon-button chat-sidebar-toggle" type="button" onClick={() => {
					onSidebarCollapsedChange(!sidebarCollapsed)
				}} aria-controls={sidebarContentID} aria-expanded={sidebarVisible} aria-label={sidebarCollapsed ? 'Show chats sidebar' : 'Hide chats sidebar'} title={sidebarCollapsed ? 'Show chats' : 'Hide chats'}>
					{sidebarCollapsed ? <PanelLeftOpen size={16} /> : <PanelLeftClose size={16} />}
				</button>
			</div>
			<div className="chat-sidebar-rail" aria-hidden={!sidebarCollapsed}>
				<button className="chat-sidebar-rail-button" type="button" onClick={(event) => openCreateDialog(true, event.currentTarget)} disabled={availableInstances.length === 0 || creating} aria-label="Create new chat" title="New chat" aria-haspopup="dialog" aria-expanded={createDialog.open && createDialog.preserveCollapsed}>
					{creating ? <LoaderCircle size={16} className="spin" /> : <Plus size={17} />}
				</button>
				<div className="chat-sidebar-rail-divider" />
				<div className="chat-sidebar-rail-sessions" aria-label="Chat sessions">
					{loading ? <LoaderCircle size={15} className="spin chat-sidebar-rail-loading" /> : sessions.map((session) => {
						const unread = selectedID !== session.id && session.last_message_role === 'assistant' && Boolean(session.last_message_id) && seenAssistantMessages[session.id] !== session.last_message_id
						const messageTime = session.last_message_at || session.updated_at
						const displayedTime = chatRailTimestamp(messageTime)
						const identityCode = chatIdentityCode(session)
						return <button key={session.id} className={`chat-sidebar-rail-button chat-sidebar-rail-session${selectedID === session.id ? ' chat-sidebar-rail-active' : ''}`} type="button" onClick={() => selectSession(session)} aria-label={`${session.title}, chat ${identityCode}, ${displayedTime.label}${session.response_in_progress ? ', working' : ''}${unread ? ', new response' : ''}`} title={`${session.title} · ${identityCode} · ${fullChatTimestamp(messageTime)}`}>
							<MessageCircle size={16} />
							<span className="chat-sidebar-rail-code" aria-hidden="true">{identityCode}</span>
							<time className="chat-sidebar-rail-time" dateTime={messageTime}>{displayedTime.date && <span className="chat-sidebar-rail-date">{displayedTime.date}</span>}<span className="chat-sidebar-rail-clock">{displayedTime.time}</span></time>
							{session.response_in_progress && <LoaderCircle size={10} className="spin chat-sidebar-rail-status" aria-hidden="true" />}
							{unread && !session.response_in_progress && <span className="chat-sidebar-rail-unread" aria-hidden="true" />}
						</button>
					})}
				</div>
			</div>
			<div id={sidebarContentID} className="chat-sidebar-content" aria-hidden={!sidebarVisible}>
			<div className="chat-create-row"><button className="primary-button" onClick={(event) => openCreateDialog(false, event.currentTarget)} disabled={availableInstances.length === 0 || creating} aria-haspopup="dialog" aria-expanded={createDialog.open && !createDialog.preserveCollapsed}><Plus size={16} />New chat</button></div>
			<div className="chat-session-list" aria-label="Chat sessions">{loading ? <span className="chat-list-note">Loading sessions</span> : sessions.length === 0 ? <span className="chat-list-note">Create a session and choose its target instance.</span> : sessions.map((session) => {
				const unread = selectedID !== session.id && session.last_message_role === 'assistant' && Boolean(session.last_message_id) && seenAssistantMessages[session.id] !== session.last_message_id
				const messageTime = session.last_message_at || session.updated_at
				const confirmingDelete = deleteConfirmationID === session.id
				const deleting = deletingID === session.id
				const sessionAttentionLabel = session.last_error?.toLowerCase().startsWith('tool stalled:') ? 'Tool stalled' : 'Needs attention'
				return <div key={session.id} className={`chat-session-item${selectedID === session.id ? ' chat-session-active' : ''}${unread ? ' chat-session-unread' : ''}${confirmingDelete ? ' chat-session-confirming-delete' : ''}`}><button className="chat-session-select" onClick={() => selectSession(session)}><span className="chat-session-title"><strong>{session.title}</strong><time dateTime={messageTime} title={fullChatTimestamp(messageTime)}>{chatTimestamp(messageTime)}</time></span><span className="chat-session-summary"><span className="chat-session-preview">{session.last_message_preview || 'No messages yet'}</span><span className="chat-session-signals">{session.response_in_progress && <LoaderCircle size={12} className="spin" aria-label={`${session.title} is working`} />}{unread && <span className="chat-session-new" aria-label="New response">New</span>}</span></span>{session.last_error && !session.response_in_progress && <small>{sessionAttentionLabel}</small>}</button>{confirmingDelete ? <div className="chat-session-delete-actions" role="group" aria-label={`Confirm deletion of ${session.title}`}><button className="chat-session-delete-confirm" onClick={() => void deleteSession(session)} disabled={Boolean(deletingID)} autoFocus>{deleting ? <LoaderCircle size={13} className="spin" aria-label="Deleting chat" /> : session.response_in_progress ? 'Stop & delete' : 'Delete'}</button><button className="chat-session-delete-cancel" onClick={() => setDeleteConfirmationID('')} disabled={Boolean(deletingID)} aria-label={`Cancel deletion of ${session.title}`} title="Cancel"><X size={14} /></button></div> : <button className="chat-session-delete" onClick={() => setDeleteConfirmationID(session.id)} disabled={Boolean(deletingID)} aria-label={`Delete ${session.title}`} title="Delete chat"><Trash2 size={14} /></button>}</div>
			})}</div>
			</div>
		</aside>
		<div className="chat-main">
			{error && <div className="chat-error" role="alert">{error}<button className="icon-button" onClick={() => setError('')} title="Dismiss"><X size={15} /></button></div>}
			{!thread ? <EmptyState icon={MessageCircle} title="No chat selected" detail="Create a session and lock it to one Hermes instance." /> : <>
					<div className={`chat-thread-heading${editingSessionConfiguration ? ' chat-thread-heading-editing' : ''}`}><div className="chat-thread-identity"><h2>{thread.session.title}</h2>{editingSessionConfiguration ? <form className="chat-session-configuration-form" onSubmit={(event) => void saveSessionConfiguration(event)}><label>Model<select aria-label="Session model" value={sessionConfiguration.model} onChange={(event) => setSessionConfiguration((current) => ({ ...current, model: event.target.value }))} disabled={savingSessionConfiguration || responseInProgress} required>{sessionModelOptions.map((model) => <option key={model} value={model}>{model}</option>)}</select></label><label>Reasoning<select aria-label="Session reasoning" value={sessionConfiguration.reasoning} onChange={(event) => setSessionConfiguration((current) => ({ ...current, reasoning: event.target.value }))} disabled={savingSessionConfiguration || responseInProgress} required><option value="none">None</option><option value="minimal">Minimal</option><option value="low">Low</option><option value="medium">Medium</option><option value="high">High</option><option value="xhigh">Xhigh</option></select></label><label>Service tier<select aria-label="Session service tier" value={sessionConfiguration.service_tier} onChange={(event) => setSessionConfiguration((current) => ({ ...current, service_tier: event.target.value }))} disabled={savingSessionConfiguration || responseInProgress} required><option value="normal">Normal</option><option value="priority">Priority</option></select></label><div className="chat-session-configuration-actions"><button type="button" className="icon-button" onClick={() => {
					setSessionConfiguration({ model: thread.session.model, reasoning: thread.session.reasoning, service_tier: thread.session.service_tier })
					setEditingSessionConfiguration(false)
				}} disabled={savingSessionConfiguration} aria-label="Cancel session configuration"><X size={15} /></button><button type="submit" className="icon-button chat-session-configuration-save" disabled={!sessionConfigurationChanged || savingSessionConfiguration || responseInProgress} aria-label="Save session configuration">{savingSessionConfiguration ? <LoaderCircle size={15} className="spin" /> : <Check size={15} />}</button></div></form> : <dl className="chat-thread-metadata" aria-label="Hermes session configuration"><div className="chat-session-id"><dt>ID</dt><dd title={thread.session.id}>{thread.session.id.slice(0, Math.ceil(thread.session.id.length / 2))}…</dd><button type="button" className="chat-session-id-copy" onClick={() => void copySessionID()} aria-label={copiedSessionID === thread.session.id ? 'Session ID copied' : 'Copy full session ID'} title={copiedSessionID === thread.session.id ? 'Copied' : 'Copy full session ID'}>{copiedSessionID === thread.session.id ? <Check size={11} /> : <Copy size={11} />}</button><span className="sr-only" aria-live="polite">{copiedSessionID === thread.session.id ? 'Session ID copied' : ''}</span></div><div><dt className="chat-thread-metadata-icon" title="Instance"><img src="/hermes-logo.png" alt="" /><span className="sr-only">Instance</span></dt><dd>{thread.session.instance_name}</dd></div><div><dt className="chat-thread-metadata-icon" title="Model"><Bot size={12} aria-hidden="true" /><span className="sr-only">Model</span></dt><dd>{thread.session.model || 'Not configured'}</dd></div><div><dt className="chat-thread-metadata-icon" title="Reasoning"><Brain size={12} aria-hidden="true" /><span className="sr-only">Reasoning</span></dt><dd>{thread.session.reasoning || 'Not configured'}</dd></div><div><dt className="chat-thread-metadata-icon" title="Service tier"><Zap size={12} aria-hidden="true" /><span className="sr-only">Service tier</span></dt><dd>{thread.session.service_tier || 'Not configured'}</dd></div></dl>}</div><div className="chat-heading-status">{streamState === 'RECONNECTING' && <span className="chat-live-state chat-live-reconnecting">Reconnecting</span>}{thread.session.last_error && !responseInProgress ? <Status value="FAILED" label={thread.session.last_error.toLowerCase().startsWith('tool stalled:') ? 'Tool stalled' : 'Needs attention'} /> : null}{!editingSessionConfiguration && <button className="icon-button chat-output-shortcut" onClick={() => onOpenOutputs(thread.session.instance_id)} aria-label={`Open outputs for ${thread.session.instance_name}`} title={`Open outputs for ${thread.session.instance_name}`}><FileOutput size={15} /></button>}{!editingSessionConfiguration && <button className="icon-button chat-session-configuration-edit" onClick={() => {
					setSessionConfiguration({ model: thread.session.model, reasoning: thread.session.reasoning, service_tier: thread.session.service_tier })
					setEditingSessionConfiguration(true)
				}} disabled={responseInProgress} aria-label="Edit session configuration" title={responseInProgress ? 'Wait for the active Hermes response' : 'Edit session configuration'}><Settings size={15} /></button>}</div></div>
				<Suspense fallback={<div className="chat-runtime-loading"><LoaderCircle size={18} className="spin" /></div>}><FleetAssistantThread key={thread.session.id} messages={thread.messages} events={thread.events ?? []} optimisticMessage={optimisticMessage} liveResponse={liveResponse} responseInProgress={responseInProgress} sending={sending} canceling={canceling} instanceName={thread.session.instance_name} token={token} formatTimestamp={chatTimestamp} onSend={sendMessage} onCancel={cancelResponse} /></Suspense>
			</>}
		</div>
		{createDialog.open && <CreateChatPopover instances={availableInstances} creatingInstanceID={creatingInstanceID} error={createError} top={createDialog.top} left={createDialog.left} onClose={closeCreateDialog} onSelect={(instanceID) => void createSession(instanceID, createDialog.preserveCollapsed)} />}
	</section>
}

function CreateChatPopover({ instances, creatingInstanceID, error, top, left, onClose, onSelect }: {
	instances: Instance[]
	creatingInstanceID: string
	error: string
	top: number
	left: number
	onClose: () => void
	onSelect: (instanceID: string) => void
}) {
	const popoverRef = useRef<HTMLDivElement>(null)
	useEffect(() => {
		popoverRef.current?.querySelector<HTMLButtonElement>('button')?.focus()
		const closeOnPointerDown = (event: PointerEvent) => {
			if (!creatingInstanceID && !popoverRef.current?.contains(event.target as Node)) onClose()
		}
		const closeOnEscape = (event: KeyboardEvent) => {
			if (event.key === 'Escape' && !creatingInstanceID) onClose()
		}
		document.addEventListener('pointerdown', closeOnPointerDown)
		document.addEventListener('keydown', closeOnEscape)
		return () => {
			document.removeEventListener('pointerdown', closeOnPointerDown)
			document.removeEventListener('keydown', closeOnEscape)
		}
	}, [creatingInstanceID, onClose])
	return createPortal(<div ref={popoverRef} className="chat-new-popover" role="dialog" aria-label="Choose Hermes instance" aria-busy={Boolean(creatingInstanceID)} style={{ top, left }}>
		<div className="chat-new-popover-heading"><strong>New chat</strong><span>Choose an instance</span></div>
		<div className="chat-new-instance-list">{instances.map((instance) => <button key={instance.id} type="button" onClick={() => onSelect(instance.id)} disabled={Boolean(creatingInstanceID)}>
			<span className="chat-new-instance-icon"><Server size={15} /></span><span><strong>{instance.name}</strong><small>Hermes {instance.hermes_version || 'managed instance'}</small></span>{creatingInstanceID === instance.id ? <LoaderCircle size={15} className="spin" /> : <ChevronRight size={15} />}
		</button>)}</div>
		{error && <div className="chat-new-popover-error">{error}</div>}
	</div>, document.body)
}

function Login({ onLogin }: { onLogin: (token: string) => void }) {
  const [value, setValue] = useState('')
  return (
    <div className="login-page">
      <div className="login-panel">
        <div className="brand login-brand"><div className="brand-mark"><Boxes size={20} /></div><div><strong>Hermes Fleet</strong><span>Operator access</span></div></div>
        <form onSubmit={(event) => { event.preventDefault(); if (value.trim()) onLogin(value.trim()) }}>
          <label htmlFor="admin-token">Admin token</label>
          <div className="input-with-icon"><KeyRound size={18} /><input id="admin-token" type="password" value={value} onChange={(event) => setValue(event.target.value)} autoComplete="current-password" autoFocus /></div>
          <button className="primary-button full-button" type="submit" disabled={!value.trim()}>Sign in<ChevronRight size={18} /></button>
        </form>
      </div>
    </div>
  )
}

function FleetView({ hosts, instances, operations, counts, remoteRoutes, publicationAttentionInstanceIDs, token, onChanged, onOperation, loading, hasMoreOperations, onSelect }: {
  hosts: Host[]
  instances: Instance[]
  operations: Operation[]
  counts: { onlineHosts: number; runningInstances: number; attentionInstances: number }
	remoteRoutes: Record<string, RemoteAccessPublishedRoute>
	publicationAttentionInstanceIDs: Set<string>
  token: string
  onChanged: () => Promise<void>
  onOperation: (operation: Operation) => void
  loading: boolean
  hasMoreOperations: boolean
  onSelect: (instanceID: string) => void
}) {
  return (
    <>
      <section className="metrics-band" aria-label="Instance summary">
        <Metric icon={Server} label="Hosts online" value={`${counts.onlineHosts}/${hosts.length}`} tone="green" />
        <Metric icon={Boxes} label="Instances running" value={`${counts.runningInstances}/${instances.length}`} tone="blue" />
		<Metric icon={Activity} label="Needs attention" value={String(counts.attentionInstances)} tone={counts.attentionInstances ? 'red' : 'neutral'} />
		<Metric icon={History} label="Recent operations" value={`${groupOperations(operations).length}${hasMoreOperations ? '+' : ''}`} tone="amber" />
      </section>
      <section className="section-block">
        <div className="section-heading"><div><h2>Instances</h2><p>{instances.length} {plural(instances.length, 'instance')}</p></div>{loading && <span className="muted">Refreshing</span>}</div>
        <InstancesTable instances={instances} remoteRoutes={remoteRoutes} publicationAttentionInstanceIDs={publicationAttentionInstanceIDs} token={token} onChanged={onChanged} onOperation={onOperation} onSelect={onSelect} />
      </section>
    </>
  )
}

function Metric({ icon: Icon, label, value, tone }: { icon: typeof Server; label: string; value: string; tone: string }) {
  return <div className="metric"><div className={`metric-icon ${tone}`}><Icon size={18} /></div><div><span>{label}</span><strong>{value}</strong></div></div>
}

function InstancesTable({ instances, remoteRoutes, publicationAttentionInstanceIDs, token, onChanged, onOperation, onSelect }: { instances: Instance[]; remoteRoutes: Record<string, RemoteAccessPublishedRoute>; publicationAttentionInstanceIDs: Set<string>; token: string; onChanged: () => Promise<void>; onOperation: (operation: Operation) => void; onSelect: (instanceID: string) => void }) {
  const [busyInstances, setBusyInstances] = useState<Set<string>>(() => new Set())
  const [rowErrors, setRowErrors] = useState<Record<string, string>>({})
  const busyInstancesRef = useRef<Set<string>>(new Set())
  const actionControllers = useRef<Map<string, AbortController>>(new Map())

  useEffect(() => () => {
    actionControllers.current.forEach((controller) => controller.abort())
    actionControllers.current.clear()
    busyInstancesRef.current.clear()
  }, [])

  const action = async (instance: Instance, name: 'retry' | 'start' | 'stop' | 'repair-runtime' | 'delete' | 'retry-delete-cleanup') => {
    if (busyInstancesRef.current.has(instance.id)) return
    if (name === 'delete' && !window.confirm(`Delete ${instance.name}? Its data volume will be preserved.`)) return
    if (name === 'repair-runtime' && !window.confirm(`Repair and verify ${instance.name}? Fleet will preserve its data, repair the managed services, and confirm Hermes and dashboard health.`)) return
    busyInstancesRef.current.add(instance.id)
    setBusyInstances(new Set(busyInstancesRef.current))
    setRowErrors((current) => ({ ...current, [instance.id]: '' }))
    const controller = new AbortController()
    actionControllers.current.set(instance.id, controller)
    try {
      const path = name === 'retry-delete-cleanup'
        ? `/api/v1/instances/${instance.id}/delete-cleanup/retry`
        : `/api/v1/instances/${instance.id}/actions`
      let operation = await apiRequest<Operation>(token, path, {
        method: 'POST',
        body: JSON.stringify(name === 'retry-delete-cleanup' ? {} : { action: name, confirm_name: ['delete', 'repair-runtime'].includes(name) ? instance.name : '' }),
        signal: controller.signal,
      })
      onOperation(operation)
      if (['PENDING', 'RUNNING'].includes(operation.status)) {
        operation = await waitForOperation(token, operation.id, controller.signal)
        onOperation(operation)
      }
      if (operation.status === 'FAILED') throw new Error(operation.error || `${operation.summary} failed`)
      await onChanged()
    } catch (requestError) {
      if (requestError instanceof DOMException && requestError.name === 'AbortError') return
      setRowErrors((current) => ({
        ...current,
        [instance.id]: requestError instanceof Error ? requestError.message : 'Lifecycle action failed',
      }))
    } finally {
      if (actionControllers.current.get(instance.id) === controller) {
        actionControllers.current.delete(instance.id)
        busyInstancesRef.current.delete(instance.id)
        setBusyInstances(new Set(busyInstancesRef.current))
      }
    }
  }

  if (instances.length === 0) {
    return <EmptyState icon={Boxes} title="No instances" detail="Create an isolated Hermes instance on an enrolled host." />
  }
  return (
    <div className="table-wrap instance-table-wrap">
      <table className="provider-table instance-table">
		<thead><tr><th>Instance</th><th>Dashboard</th><th>Hermes</th><th>Status</th><th><span className="sr-only">Actions</span></th></tr></thead>
        <tbody>
          {instances.map((instance) => {
            const busy = busyInstances.has(instance.id)
            const rowError = rowErrors[instance.id]
			const route = remoteRoutes[instance.id]
			const dashboardHostname = route?.hostname || instance.public_hostname || ''
			const dashboardURL = instance.public_dashboard_url || (route?.published && route.hostname ? `https://${route.hostname}` : '')
			const publicationConfigured = Boolean(dashboardHostname)
			const statusItems: InstanceStatusItem[] = [
				{ label: 'DNS', value: route?.dns_state === 'ready' ? 'READY' : route?.dns_state === 'failed' || route?.dns_state === 'conflict' ? 'FAILED' : 'UNKNOWN', detail: publicationConfigured ? remoteResourceStateLabel(route?.dns_state) : 'Not configured' },
				{ label: 'Route', value: route?.route_state === 'ready' ? 'READY' : route?.route_state === 'failed' || route?.route_state === 'conflict' ? 'FAILED' : 'UNKNOWN', detail: publicationConfigured ? remoteResourceStateLabel(route?.route_state) : 'Not configured' },
				{ label: 'Endpoint', value: route?.endpoint_state === 'reachable' ? 'READY' : route?.endpoint_state === 'unavailable' || route?.endpoint_state === 'access_protected' ? 'FAILED' : 'UNKNOWN', detail: publicationConfigured ? remoteRouteEndpointLabel(route?.endpoint_state) : 'Not configured' },
				{ label: 'Runtime', value: instance.status, detail: runtimeStatusLabel(instance.status) },
				{ label: 'Readiness', value: instanceReadinessStatus(instance), detail: healthStatusLabel(instanceReadinessStatus(instance)) },
			]
			const publicationNeedsAttention = publicationConfigured && (publicationAttentionInstanceIDs.has(instance.id) || statusItems.slice(0, 3).some((item) => item.value === 'FAILED'))
			const lifecycleChecking = ['PROVISIONING', 'RESTARTING', 'UPDATING', 'RECONCILING', 'BACKING_UP', 'RESTORING', 'DELETING'].includes(instance.status)
			const runtimeNeedsAttention = !lifecycleChecking && (!['RUNNING', 'STOPPED'].includes(instance.status) || !['IN_SYNC', 'OK'].includes(instanceOperationalHealthStatus(instance)))
			const setupNeedsAttention = !lifecycleChecking && instanceReadinessStatus(instance) === 'INCOMPLETE'
			const statusChecking = lifecycleChecking || (publicationConfigured && statusItems.slice(0, 3).some((item) => item.value === 'UNKNOWN'))
			const statusSummary = instance.status === 'DELETING' && instance.last_error
				? { value: 'FAILED', label: 'Cleanup required' }
				: lifecycleChecking
				? { value: 'PENDING', label: instance.status === 'DELETING' ? 'Deleting' : 'Checking' }
				: publicationNeedsAttention || runtimeNeedsAttention
					? { value: 'FAILED', label: 'Needs attention' }
					: setupNeedsAttention
						? { value: 'INCOMPLETE', label: 'Setup incomplete' }
					: statusChecking
						? { value: 'PENDING', label: 'Checking' }
						: { value: 'READY', label: 'Healthy' }
            const runtimeDrift = instance.observation?.checks?.some((check) =>
              ['runtime', 'health_endpoint', 'dashboard_endpoint'].includes(check.name) &&
              ['DRIFT', 'MISSING'].includes(check.status),
            )
            return (
            <tr key={instance.id}>
              <td data-label="Instance"><button className="instance-link" onClick={() => onSelect(instance.id)}>{instance.name}</button></td>
			  <td data-label="Dashboard">{dashboardURL ? <a className="instance-dashboard-link" href={dashboardURL} target="_blank" rel="noreferrer">{dashboardHostname || dashboardURL}<ExternalLink size={13} /></a> : <span className="instance-dashboard-empty">{dashboardHostname || 'Not configured'}</span>}</td>
			  <td data-label="Hermes" title={installedHermesVersion(instance)}><strong>{installedHermesVersion(instance)}</strong>{!installedHermesVersionVerified(instance) && installedHermesVersion(instance) !== 'Detecting' && <span className="secondary-text">Verifying</span>}</td>
	              <td data-label="Status"><InstanceStatusSummary instanceName={instance.name} items={statusItems} summaryValue={statusSummary.value} summaryLabel={statusSummary.label} />{rowError && <span className="error-text">Action needs review</span>}{instance.last_error && instanceOperationalHealthStatus(instance) !== 'IN_SYNC' && <span className="error-text" title={instance.last_error}>Previous action failed</span>}</td>
              <td data-label="Actions">
                <div className="row-actions">
                  {instance.status === 'RUNNING' && runtimeDrift && (!instance.runtime_remediation || ['CANCELED', 'EXHAUSTED'].includes(instance.runtime_remediation.status)) && <button className="icon-button repair-button" onClick={() => void action(instance, 'repair-runtime')} disabled={busy} title="Repair and verify runtime"><RefreshCw size={17} className={busy ? 'spin' : ''} /></button>}
                  {instance.status === 'STOPPED' && <button className="icon-button" onClick={() => void action(instance, 'start')} disabled={busy} title="Start instance"><Play size={17} /></button>}
                  {instance.status === 'FAILED' && <button className="icon-button" onClick={() => onSelect(instance.id)} disabled={busy} title="Review recovery options"><RefreshCw size={17} /></button>}
                  {instance.status === 'DELETING' && <button className="icon-button repair-button" onClick={() => void action(instance, 'retry-delete-cleanup')} disabled={busy || !instance.last_error} title={instance.last_error ? 'Retry Cloudflare cleanup' : 'Cloudflare cleanup in progress'}><RefreshCw size={17} className={busy || !instance.last_error ? 'spin' : ''} /></button>}
                  {instance.status === 'RUNNING' && <button className="icon-button" onClick={() => void action(instance, 'stop')} disabled={busy} title="Stop instance"><CircleStop size={17} /></button>}
                  {['RUNNING', 'STOPPED', 'FAILED'].includes(instance.status) && <button className="icon-button danger-button" onClick={() => void action(instance, 'delete')} disabled={busy} title="Delete instance and preserve data"><Trash2 size={17} /></button>}
                </div>
              </td>
            </tr>
          )})}
        </tbody>
      </table>
    </div>
  )
}

function InstanceProfile({
	instance,
	operations,
	token,
	onChanged,
	onOperation,
	refreshSignal,
}: {
	instance: Instance
	operations: Operation[]
	token: string
	onChanged: () => Promise<void>
	onOperation: (operation: Operation) => void
	refreshSignal: number
}) {
  const [credentials, setCredentials] = useState<Credentials | null>(null)
  const [expiresAt, setExpiresAt] = useState('')
  const [revealing, setRevealing] = useState(false)
  const [credentialOperation, setCredentialOperation] = useState<Operation | null>(null)
  const credentialPoll = useRef<AbortController | null>(null)
  const [error, setError] = useState('')
  const [codexAuthOpen, setCodexAuthOpen] = useState(false)
  const [selectedTab, setSelectedTab] = useState<InstanceTab>(() => readStoredInstanceTab(instance.id))
  const [showPassedDiagnostics, setShowPassedDiagnostics] = useState(false)
  const [refreshingObservation, setRefreshingObservation] = useState(false)
	const [fixingImage, setFixingImage] = useState(false)
	const [repairingRuntime, setRepairingRuntime] = useState(false)
	const [cancelingRuntimeRecovery, setCancelingRuntimeRecovery] = useState(false)
	const [syncingRuntime, setSyncingRuntime] = useState(false)
	const [configuringCodex, setConfiguringCodex] = useState(false)
	const [codexConfigurationError, setCodexConfigurationError] = useState('')
	const [codexDraft, setCodexDraft] = useState<CodexFormState | null>(null)
  const [observationError, setObservationError] = useState('')
	const [recoveryPoints, setRecoveryPoints] = useState<RecoveryPoint[]>([])
	const [recoveryBusy, setRecoveryBusy] = useState('')
	const [recoveryError, setRecoveryError] = useState('')
	const [hermesUpdate, setHermesUpdate] = useState<HermesUpdate | null>(null)
	const [hermesUpdateError, setHermesUpdateError] = useState('')
	const [hermesUpdateStartError, setHermesUpdateStartError] = useState('')
	const [pendingHermesUpdate, setPendingHermesUpdate] = useState<Operation | null>(null)
	const [startingHermesUpdate, setStartingHermesUpdate] = useState(false)
	const [remoteAccessConfiguration, setRemoteAccessConfiguration] = useState<RemoteAccessConfiguration | null>(null)
	const [remoteAccessError, setRemoteAccessError] = useState('')
	const [publicationOperation, setPublicationOperation] = useState<Operation | null>(null)
	const [publicationBusy, setPublicationBusy] = useState(false)

  useEffect(() => {
    try {
      window.localStorage.setItem(`${instanceTabStoragePrefix}${instance.id}`, selectedTab)
    } catch {
      // Keep the selected tab in memory when browser storage is unavailable.
    }
  }, [instance.id, selectedTab])
	const publicationController = useRef<AbortController | null>(null)
	const [activeAction, setActiveAction] = useState('')
	const recoveryLoadController = useRef<AbortController | null>(null)
	const recoveryLoadSequence = useRef(0)
	const hermesUpdateLoadController = useRef<AbortController | null>(null)
	const hermesUpdateLoadSequence = useRef(0)
	const actionPollController = useRef<AbortController | null>(null)
	const activeActionRef = useRef('')
  const observation = instance.observation
  const observationChecks = observation?.checks ?? []
  const observationReady = ['RUNNING', 'STOPPED', 'FAILED'].includes(instance.status) && Boolean(instance.managed_path && instance.project_name && instance.data_volume)
	const imageDrift = observation?.status === 'DEGRADED' && observationChecks.some((check) => check.name === 'image' && check.status === 'DRIFT')
	const runtimeCheck = observationChecks.find((check) => check.name === 'runtime')
	const runtimeDrift = observationChecks.some((check) =>
		['runtime', 'health_endpoint', 'dashboard_endpoint'].includes(check.name) &&
		['DRIFT', 'MISSING'].includes(check.status),
	)
	const runtimeConfigCheck = observationChecks.find((check) => check.name === 'runtime_configuration')
	const runtimeConfigDrift = observation?.status === 'DEGRADED' && runtimeConfigCheck?.status === 'DRIFT'
	const codexAuthCheck = observationChecks.find((check) => check.name === 'codex_auth')
	const codexConnected = codexAuthCheck?.status === 'OK'
	const codexModelCatalog = observation?.model_catalog ?? []
	const recommendedCodexModel = observation?.recommended_model ?? ''
	const savedCodexForm = codexFormFromInstance(instance)
	const codexForm = codexDraft ?? savedCodexForm
	const codexDirty = codexDraft !== null
	const codexModelOptions = instance.model && !codexModelCatalog.includes(instance.model)
		? [instance.model, ...codexModelCatalog]
		: codexModelCatalog
	const hasCodexConfiguration = Boolean(instance.codex_configured && instance.model && instance.reasoning && instance.service_tier)
	const codexFormValid = Boolean(
		codexForm.model &&
		codexForm.reasoning &&
		codexForm.service_tier &&
		codexModelOptions.includes(codexForm.model),
	)
	const canFixImageDrift = imageDrift && ['RUNNING', 'STOPPED'].includes(instance.status)
	const runtimeRemediation = instance.runtime_remediation
	const automaticRuntimeRecoveryActive = Boolean(runtimeRemediation && !['CANCELED', 'EXHAUSTED'].includes(runtimeRemediation.status))
	const canRepairRuntime = runtimeDrift && instance.status === 'RUNNING' && !automaticRuntimeRecoveryActive
	const codexConfigurationActive = codexConnected && hasCodexConfiguration && runtimeConfigCheck?.status === 'OK'
	const imageRepairing = instance.status === 'RECONCILING'
	const runtimeSyncInProgress = instance.status === 'UPDATING'
	const codexSetupIssue = codexSetupDiagnostic(instance.provider, codexAuthCheck, runtimeConfigCheck, codexConnected, hasCodexConfiguration)
	const issueChecks = observationChecks.filter((check) => check.status !== 'OK' && check.name !== 'codex_auth' && check.name !== 'runtime_configuration')
	const summarizedIssueChecks = runtimeCheck && ['DRIFT', 'MISSING'].includes(runtimeCheck.status)
		? issueChecks.filter((check) => check.name === 'runtime' || check.name !== 'containers')
		: issueChecks
	const operationalIssueChecks = summarizedIssueChecks
	const setupItems = codexSetupIssue ? [codexSetupIssue] : []
	const diagnosticChecks = [...setupItems, ...operationalIssueChecks]
	const passedChecks = observationChecks.filter((check) => check.status === 'OK')
	const visibleDiagnostics = showPassedDiagnostics ? [...diagnosticChecks, ...passedChecks] : diagnosticChecks
	const codexSetupTitle = codexSetupIssueTitle(codexConnected, hasCodexConfiguration, runtimeConfigDrift)
	const effectiveOperationalHealth = instanceOperationalHealthStatus(instance)
	const effectiveOperationalSummary = operationalHealthSummary(instance)
	const effectiveInstalledVersion = installedHermesVersion(instance)
	const installedVersionVerified = installedHermesVersionVerified(instance)
	const updateOperations = pendingHermesUpdate && !operations.some((operation) => operation.id === pendingHermesUpdate.id)
		? [pendingHermesUpdate, ...operations]
		: operations
	const trackedInstanceOperation = updateOperations.some((operation) =>
		operation.instance_id === instance.id && ['PENDING', 'RUNNING'].includes(operation.status),
	)
	const instanceActionBusy = activeAction !== '' || trackedInstanceOperation
	const activeInstanceOperationCount = groupOperations(updateOperations).filter((group) => ['PENDING', 'RUNNING'].includes(group.status)).length
	const latestHermesUpdateOperation = updateOperations
		.filter((operation) => operation.type === 'UPGRADE_HERMES')
		.sort((left, right) => new Date(right.created_at).getTime() - new Date(left.created_at).getTime())[0]
	const hermesUpdateFlow = persistentHermesUpdateFlow(latestHermesUpdateOperation)
	const versionUpdateAvailable = Boolean(
		hermesUpdate?.available &&
		hermesUpdate.eligible &&
		hermesUpdate.update_kind === 'VERSION_UPDATE' &&
		hermesUpdate.official_status === 'UPDATE_AVAILABLE',
	)
	const runtimeRefreshRequired = Boolean(
		hermesUpdate?.available &&
		hermesUpdate.update_kind === 'RUNTIME_REFRESH' &&
		hermesUpdate.official_status === 'CURRENT',
	)
	const failedRuntimeRecoveryAvailable = Boolean(
		instance.status === 'FAILED' &&
		runtimeRefreshRequired &&
		hermesUpdate?.eligible,
	)
	const failedRuntimeRecoveryNeedsVerification = Boolean(
		instance.status === 'FAILED' &&
		runtimeRefreshRequired &&
		!hermesUpdate?.eligible,
	)
	const manualRuntimeRecoveryRequired = Boolean(
		instance.status === 'FAILED' &&
		instance.last_error?.includes('manual recovery is required'),
	)
	const failedProvisioningRetryAvailable = Boolean(
		instance.status === 'FAILED' &&
		!manualRuntimeRecoveryRequired &&
		hermesUpdate !== null &&
		!runtimeRefreshRequired,
	)
	const canRunHermesMaintenance = Boolean(
		hermesUpdate?.available &&
		hermesUpdate.eligible &&
		(
			['RUNNING', 'STOPPED'].includes(instance.status) ||
			failedRuntimeRecoveryAvailable
		),
	)
	const canSyncRuntime = runtimeConfigDrift && hasCodexConfiguration && !runtimeRefreshRequired &&
		['RUNNING', 'STOPPED'].includes(instance.status)
	const updatePanelRuntimeRefresh = hermesUpdateFlow.status !== 'idle'
		? hermesUpdateFlow.kind === 'RUNTIME_REFRESH'
		: runtimeRefreshRequired
	const updatePanelTargetVersion = hermesUpdateFlow.status !== 'idle' ? hermesUpdateFlow.targetVersion : hermesUpdate?.target_version ?? ''
	const updatePanelTitle = hermesUpdateFlow.status === 'error'
		? updatePanelRuntimeRefresh ? 'Managed runtime maintenance stopped' : 'Hermes update stopped'
		: hermesUpdateFlow.status === 'running'
			? updatePanelRuntimeRefresh ? `Refreshing managed runtime for Hermes ${updatePanelTargetVersion}` : `Updating Hermes to ${updatePanelTargetVersion}`
			: failedRuntimeRecoveryAvailable
				? 'Managed runtime recovery available'
				: failedRuntimeRecoveryNeedsVerification
					? 'Verify retained runtime'
				: updatePanelRuntimeRefresh ? 'Managed runtime refresh required' : `Update available: Hermes ${updatePanelTargetVersion}`
	const updatePanelActionLabel = startingHermesUpdate
		? updatePanelRuntimeRefresh ? 'Queuing maintenance' : 'Queuing update'
		: failedRuntimeRecoveryAvailable
			? 'Recover managed runtime'
		: updatePanelRuntimeRefresh
			? hermesUpdateFlow.status === 'error' ? 'Retry maintenance' : 'Refresh managed runtime'
			: `Update to ${updatePanelTargetVersion}`
	const effectiveCodexSetupTitle = runtimeRefreshRequired && runtimeConfigDrift
		? 'Refresh managed runtime before applying Codex'
		: codexSetupTitle
	const instanceTabs: { id: InstanceTab; label: string }[] = [
		{ id: 'overview', label: 'Overview' },
		{ id: 'access', label: 'Access' },
		{ id: 'configuration', label: 'Codex' },
		{ id: 'profiles', label: 'Profiles' },
		{ id: 'messaging', label: 'Messaging' },
		{ id: 'mcp', label: 'MCP' },
		{ id: 'recovery', label: 'Backups' },
		{ id: 'diagnostics', label: 'Diagnostics' },
		{ id: 'operations', label: 'Operations' },
	]
	const instancePublishedRoute = remoteAccessConfiguration?.instance_routes?.find((route) => route.instance_id === instance.id)
	const externalDashboardEndpoint = remoteAccessConfiguration?.instance_endpoints?.find((endpoint) => endpoint.instance_id === instance.id)
	const managedRoutePublished = instancePublishedRoute?.published === true
	const managedRouteRevalidating = managedRoutePublished && instancePublishedRoute?.revalidating === true
	const managedPublicDashboardURL = managedRoutePublished && instancePublishedRoute?.hostname ? `https://${instancePublishedRoute.hostname}` : ''
	const effectivePublicDashboardURL = instance.public_dashboard_url || managedPublicDashboardURL || externalDashboardEndpoint?.dashboard_url || ''
	const publicDashboardOrigin = instancePublishedRoute?.origin_service || `http://hermes-fleet-instance-${instance.name}-dashboard:9119`
	const publishedHostname = instancePublishedRoute?.hostname || instance.public_hostname || ''
	const instancePublishingZone = (remoteAccessConfiguration?.instance_publishing_zone ?? '').replace(/^\.+|\.+$/g, '')
	const instancePublishingNamespace = remoteAccessConfiguration?.instance_publishing_fleet_namespace ?? ''
	const generatedPublicHostname = instancePublishingNamespace && instancePublishingZone
		? `${instancePublishingNamespace}-${instance.name}.${instancePublishingZone}`
		: ''
	const effectivePublishingHostname = publishedHostname || generatedPublicHostname

	const updateCodexForm = (key: keyof CodexFormState, value: string) => {
		const next = { ...codexForm, [key]: value }
		setCodexDraft(codexFormsEqual(next, savedCodexForm) ? null : next)
	}

	const loadRecoveryPoints = useCallback(async () => {
		const sequence = recoveryLoadSequence.current + 1
		recoveryLoadSequence.current = sequence
		recoveryLoadController.current?.abort()
		const controller = new AbortController()
		recoveryLoadController.current = controller
		try {
			const items = await apiRequest<RecoveryPoint[]>(token, `/api/v1/instances/${instance.id}/recovery-points`, { signal: controller.signal })
			if (controller.signal.aborted || sequence !== recoveryLoadSequence.current) return
			setRecoveryPoints(items ?? [])
			setRecoveryError('')
		} catch (requestError) {
			if (requestError instanceof DOMException && requestError.name === 'AbortError') return
			if (sequence !== recoveryLoadSequence.current) return
			setRecoveryError(requestError instanceof Error ? requestError.message : 'Backups could not be loaded')
		} finally {
			if (recoveryLoadController.current === controller) recoveryLoadController.current = null
		}
	}, [instance.id, token])

	const loadHermesUpdate = useCallback(async () => {
		const sequence = hermesUpdateLoadSequence.current + 1
		hermesUpdateLoadSequence.current = sequence
		hermesUpdateLoadController.current?.abort()
		const controller = new AbortController()
		hermesUpdateLoadController.current = controller
		try {
			const update = await apiRequest<HermesUpdate>(token, `/api/v1/instances/${instance.id}/hermes-update`, { signal: controller.signal })
			if (controller.signal.aborted || sequence !== hermesUpdateLoadSequence.current) return
			setHermesUpdate(update)
			setHermesUpdateError('')
		} catch (requestError) {
			if (requestError instanceof DOMException && requestError.name === 'AbortError') return
			if (sequence !== hermesUpdateLoadSequence.current) return
			setHermesUpdateError(requestError instanceof Error ? requestError.message : 'Hermes update status could not be loaded')
		} finally {
			if (hermesUpdateLoadController.current === controller) hermesUpdateLoadController.current = null
		}
	}, [instance.id, token])

	const loadRemoteAccessConfiguration = useCallback(async () => {
		try {
			const value = await apiRequest<RemoteAccessConfiguration>(token, '/api/v1/system/remote-access/configuration', { cache: 'no-store' })
			setRemoteAccessConfiguration(value)
			setRemoteAccessError('')
		} catch (requestError) {
			setRemoteAccessConfiguration(null)
			setRemoteAccessError(requestError instanceof Error ? requestError.message : 'Public dashboard routing could not be loaded')
		}
	}, [token])
	useEffect(() => () => publicationController.current?.abort(), [])

	const publishDashboard = async (hostname: string) => {
		if (publicationBusy) return
		publicationController.current?.abort()
		const controller = new AbortController()
		publicationController.current = controller
		setPublicationBusy(true)
		setRemoteAccessError('')
		try {
			let operation = await apiRequest<Operation>(token, `/api/v1/instances/${instance.id}/public-dashboard`, {
				method: 'PUT',
				body: JSON.stringify({ public_hostname: hostname }),
				signal: controller.signal,
			})
			setPublicationOperation(operation)
			onOperation(operation)
			operation = await waitForOperationResult(token, operation.id, controller.signal, (current) => {
				setPublicationOperation(current)
				onOperation(current)
			})
			if (operation.status === 'FAILED') {
				throw new Error(operation.progress?.detail || operation.error || `${operation.summary} failed`)
			}
			await Promise.all([onChanged(), loadRemoteAccessConfiguration()])
		} catch (requestError) {
			if (requestError instanceof DOMException && requestError.name === 'AbortError') return
			setRemoteAccessError(requestError instanceof Error ? requestError.message : 'Dashboard publication failed')
		} finally {
			if (publicationController.current === controller) publicationController.current = null
			setPublicationBusy(false)
		}
	}

	useEffect(() => {
		if (selectedTab !== 'access') return
		const initial = window.setTimeout(() => void loadRemoteAccessConfiguration(), 0)
		return () => window.clearTimeout(initial)
	}, [loadRemoteAccessConfiguration, refreshSignal, selectedTab])

	useEffect(() => {
		let stopped = false
		let timer = 0
		const poll = async () => {
			await loadRecoveryPoints()
			if (!stopped) timer = window.setTimeout(() => void poll(), 5000)
		}
		const initial = window.setTimeout(() => void poll(), 0)
		return () => {
			stopped = true
			window.clearTimeout(initial)
			window.clearTimeout(timer)
			recoveryLoadController.current?.abort()
		}
	}, [loadRecoveryPoints, refreshSignal])

	useEffect(() => {
		let stopped = false
		let timer = 0
		const poll = async () => {
			await loadHermesUpdate()
			if (!stopped) timer = window.setTimeout(() => void poll(), 5000)
		}
		const initial = window.setTimeout(() => void poll(), 0)
		return () => {
			stopped = true
			window.clearTimeout(initial)
			window.clearTimeout(timer)
			hermesUpdateLoadController.current?.abort()
		}
	}, [loadHermesUpdate, refreshSignal])

  useEffect(() => () => {
			credentialPoll.current?.abort()
			actionPollController.current?.abort()
			actionPollController.current = null
			activeActionRef.current = ''
		}, [])

  useEffect(() => {
    if (!expiresAt) return
    let timer: number | undefined
    const expireWhenDue = () => {
      const delay = new Date(expiresAt).getTime() - Date.now()
      if (delay <= 0) {
        setCredentials(null)
        setExpiresAt('')
        return
      }
      timer = window.setTimeout(expireWhenDue, Math.min(delay, 2_147_483_647))
    }
    expireWhenDue()
    return () => {
      if (timer !== undefined) window.clearTimeout(timer)
    }
  }, [expiresAt])

	  const revealCredentials = async () => {
    credentialPoll.current?.abort()
    const controller = new AbortController()
    credentialPoll.current = controller
    setRevealing(true)
    setCredentialOperation(null)
    setError('')
    try {
      const operation = await apiRequest<Operation>(token, `/api/v1/instances/${instance.id}/credentials`, { method: 'POST', body: '{}', signal: controller.signal })
      setCredentialOperation(operation)
      let delay = 500
      while (!controller.signal.aborted) {
        await sleep(delay, controller.signal)
        const result = await apiRequest<CredentialReveal | Operation>(token, `/api/v1/credential-reveals/${operation.id}`, { signal: controller.signal })
        if ('credentials' in result) {
          setCredentials(result.credentials)
          setExpiresAt(result.expires_at)
          setCredentialOperation(null)
          return
        }
        setCredentialOperation(result)
        delay = Math.min(2000, Math.round(delay * 1.5))
      }
    } catch (requestError) {
      if (requestError instanceof DOMException && requestError.name === 'AbortError') return
      setError(requestError instanceof Error ? requestError.message : 'Credentials could not be revealed')
      setCredentialOperation(null)
    } finally {
      if (credentialPoll.current === controller) {
        credentialPoll.current = null
        setRevealing(false)
      }
	    }
	  }

	const beginActionPoll = (action: string) => {
		if (actionPollController.current || activeActionRef.current) return null
		const controller = new AbortController()
		actionPollController.current = controller
		activeActionRef.current = action
		setActiveAction(action)
		return controller
	}

	const finishActionPoll = (controller: AbortController) => {
		if (actionPollController.current === controller) {
			actionPollController.current = null
			activeActionRef.current = ''
			setActiveAction('')
		}
	}

  const requestObservation = async () => {
    setRefreshingObservation(true)
    setObservationError('')
    try {
      await apiRequest(token, `/api/v1/instances/${instance.id}/observations/refresh`, { method: 'POST', body: '{}' })
      await onChanged()
    } catch (requestError) {
      setObservationError(requestError instanceof Error ? requestError.message : 'Runtime observation could not be requested')
    } finally {
      setRefreshingObservation(false)
    }
  }

	const fixImageDrift = async () => {
			if (instanceActionBusy) return
		const confirmation = instance.status === 'RUNNING'
			? `Fix image drift for ${instance.name}? Fleet will briefly stop the instance, verify the current image, restart it, and confirm health.`
			: `Fix image drift for ${instance.name}? Fleet will verify and trust the current image without starting the instance.`
			if (!window.confirm(confirmation)) return
			const controller = beginActionPoll('fix-image-drift')
			if (!controller) return
			setFixingImage(true)
			setObservationError('')
			try {
					const operation = await apiRequest<Operation>(token, `/api/v1/instances/${instance.id}/actions`, {
						method: 'POST', body: JSON.stringify({ action: 'fix-image-drift', confirm_name: instance.name }),
						signal: controller.signal,
					})
				onOperation(operation)
				if (['PENDING', 'RUNNING'].includes(operation.status)) onOperation(await waitForOperation(token, operation.id, controller.signal))
				if (operation.status === 'FAILED') throw new Error(operation.error || `${operation.summary} failed`)
				await onChanged()
			} catch (requestError) {
				if (requestError instanceof DOMException && requestError.name === 'AbortError') return
				setObservationError(requestError instanceof Error ? requestError.message : 'Automatic image repair could not be queued')
			} finally {
				finishActionPoll(controller)
				setFixingImage(false)
			}
		}

	const repairRuntime = async () => {
			if (instanceActionBusy) return
			if (!window.confirm(`Repair and verify ${instance.name}? Fleet will preserve its data, repair the managed services, and confirm Hermes and dashboard health.`)) return
			const controller = beginActionPoll('repair-runtime')
			if (!controller) return
			setRepairingRuntime(true)
			setObservationError('')
			try {
					const operation = await apiRequest<Operation>(token, `/api/v1/instances/${instance.id}/actions`, {
						method: 'POST',
						body: JSON.stringify({ action: 'repair-runtime', confirm_name: instance.name }),
						signal: controller.signal,
					})
				onOperation(operation)
				if (['PENDING', 'RUNNING'].includes(operation.status)) onOperation(await waitForOperation(token, operation.id, controller.signal))
				if (operation.status === 'FAILED') throw new Error(operation.error || `${operation.summary} failed`)
				try {
						await apiRequest(token, `/api/v1/instances/${instance.id}/observations/refresh`, { method: 'POST', body: '{}', signal: controller.signal })
				} catch {
				// The managed restart is already verified; the scheduled observation will refresh the UI.
			}
				await onChanged()
			} catch (requestError) {
				if (requestError instanceof DOMException && requestError.name === 'AbortError') return
				setObservationError(requestError instanceof Error ? requestError.message : 'Runtime repair and verification failed')
			} finally {
				finishActionPoll(controller)
				setRepairingRuntime(false)
			}
		}

	const cancelAutomaticRuntimeRecovery = async () => {
		if (!window.confirm(`Stop automatic runtime recovery for ${instance.name}? The current issue will remain visible and can still be repaired manually.`)) return
		setCancelingRuntimeRecovery(true)
		setObservationError('')
			try {
				await apiRequest(token, `/api/v1/instances/${instance.id}/runtime-remediation/cancel`, {
					method: 'POST', body: '{}',
				})
				await onChanged()
		} catch (requestError) {
			setObservationError(requestError instanceof Error ? requestError.message : 'Automatic runtime recovery could not be stopped')
		} finally {
			setCancelingRuntimeRecovery(false)
		}
	}

	const syncRuntimeConfiguration = async () => {
			if (instanceActionBusy) return
			const controller = beginActionPoll('sync-runtime')
			if (!controller) return
			setSyncingRuntime(true)
			setObservationError('')
			try {
					const operation = await apiRequest<Operation>(token, `/api/v1/instances/${instance.id}/actions`, {
						method: 'POST', body: JSON.stringify({ action: 'sync-runtime', confirm_name: instance.name }),
						signal: controller.signal,
					})
				onOperation(operation)
				if (['PENDING', 'RUNNING'].includes(operation.status)) onOperation(await waitForOperation(token, operation.id, controller.signal))
				if (operation.status === 'FAILED') throw new Error(operation.error || `${operation.summary} failed`)
				await onChanged()
			} catch (requestError) {
				if (requestError instanceof DOMException && requestError.name === 'AbortError') return
				setObservationError(requestError instanceof Error ? requestError.message : 'Hermes setup could not be completed automatically')
			} finally {
				finishActionPoll(controller)
				setSyncingRuntime(false)
			}
		}

		const configureCodex = async (event: FormEvent) => {
			event.preventDefault()
			if (!codexDirty || !codexFormValid || instanceActionBusy) return
			const controller = beginActionPoll('configure-codex')
			if (!controller) return
			setConfiguringCodex(true)
			setCodexConfigurationError('')
			try {
					const operation = await apiRequest<Operation>(token, `/api/v1/instances/${instance.id}/codex-configuration`, {
						method: 'PUT', body: JSON.stringify(codexForm),
						signal: controller.signal,
					})
				onOperation(operation)
				if (['PENDING', 'RUNNING'].includes(operation.status)) onOperation(await waitForOperation(token, operation.id, controller.signal))
				if (operation.status === 'FAILED') throw new Error(operation.error || `${operation.summary} failed`)
				await onChanged()
				setCodexDraft(null)
			} catch (requestError) {
				if (requestError instanceof DOMException && requestError.name === 'AbortError') return
				setCodexConfigurationError(requestError instanceof Error ? requestError.message : 'Codex configuration could not be applied')
			} finally {
				finishActionPoll(controller)
				setConfiguringCodex(false)
			}
		}

	const createRecoveryPoint = async () => {
		if (instanceActionBusy) return
		const controller = beginActionPoll('create-backup')
		if (!controller) return
		setRecoveryBusy('create')
		setRecoveryError('')
		try {
			const point = await apiRequest<RecoveryPoint>(token, `/api/v1/instances/${instance.id}/recovery-points`, {
				method: 'POST',
				body: '{}',
				signal: controller.signal,
			})
			if (point.operation_id) {
				const queued = await loadOperationByID(token, point.operation_id, controller.signal)
				if (queued) onOperation(queued)
				onOperation(await waitForOperation(token, point.operation_id, controller.signal))
			} else {
				await waitForRecoveryPoint(token, instance.id, point.id, controller.signal)
			}
			await onChanged()
			await loadRecoveryPoints()
		} catch (requestError) {
			if (requestError instanceof DOMException && requestError.name === 'AbortError') return
			setRecoveryError(requestError instanceof Error ? requestError.message : 'Backup could not be queued')
		} finally {
			finishActionPoll(controller)
			setRecoveryBusy('')
		}
	}

	const stopForRecovery = async () => {
		if (instanceActionBusy) return
		if (!window.confirm(`Stop ${instance.name} to create a backup?`)) return
		const controller = beginActionPoll('stop-for-backup')
		if (!controller) return
		setRecoveryBusy('stop')
		setRecoveryError('')
		try {
				const operation = await apiRequest<Operation>(token, `/api/v1/instances/${instance.id}/actions`, {
					method: 'POST', body: JSON.stringify({ action: 'stop', confirm_name: '' }),
					signal: controller.signal,
				})
			onOperation(operation)
			if (['PENDING', 'RUNNING'].includes(operation.status)) onOperation(await waitForOperation(token, operation.id, controller.signal))
			if (operation.status === 'FAILED') throw new Error(operation.error || `${operation.summary} failed`)
			await onChanged()
		} catch (requestError) {
			if (requestError instanceof DOMException && requestError.name === 'AbortError') return
			setRecoveryError(requestError instanceof Error ? requestError.message : 'Instance could not be stopped')
		} finally {
			finishActionPoll(controller)
			setRecoveryBusy('')
		}
	}

	const verifyRecoveryPoint = async (point: RecoveryPoint) => {
		setRecoveryBusy(`verify-${point.id}`)
		setRecoveryError('')
		try {
			await apiRequest<RecoveryPoint>(token, `/api/v1/recovery-points/${point.id}/verify`, { method: 'POST', body: '{}' })
			await loadRecoveryPoints()
		} catch (requestError) {
			setRecoveryError(requestError instanceof Error ? requestError.message : 'Backup verification failed')
		} finally {
			setRecoveryBusy('')
		}
	}

	const downloadRecoveryPoint = async (point: RecoveryPoint) => {
		setRecoveryBusy(`download-${point.id}`)
		setRecoveryError('')
		try {
			await apiDownloadToFile(token, `/api/v1/recovery-points/${point.id}/download`, point.filename)
		} catch (requestError) {
			setRecoveryError(requestError instanceof Error ? requestError.message : 'Backup download failed')
		} finally {
			setRecoveryBusy('')
		}
	}

	const restoreRecoveryPoint = async (point: RecoveryPoint) => {
		if (instanceActionBusy) return
		const confirmation = window.prompt(`Restore backup ${point.filename}? This replaces the stopped instance data and workspace using the compatible Hermes release installed on this host. Type ${instance.name} to continue.`)
		if (confirmation === null) return
		const controller = beginActionPoll('restore-backup')
		if (!controller) return
		setRecoveryBusy(`restore-${point.id}`)
		setRecoveryError('')
		try {
				const operation = await apiRequest<Operation>(token, `/api/v1/recovery-points/${point.id}/restore`, {
					method: 'POST', body: JSON.stringify({ confirm_name: confirmation }),
					signal: controller.signal,
				})
			onOperation(operation)
			if (['PENDING', 'RUNNING'].includes(operation.status)) onOperation(await waitForOperation(token, operation.id, controller.signal))
			if (operation.status === 'FAILED') throw new Error(operation.error || `${operation.summary} failed`)
			await onChanged()
			await loadRecoveryPoints()
		} catch (requestError) {
			if (requestError instanceof DOMException && requestError.name === 'AbortError') return
			setRecoveryError(requestError instanceof Error ? requestError.message : 'Backup could not be restored')
		} finally {
			finishActionPoll(controller)
			setRecoveryBusy('')
		}
	}

	const deleteRecoveryPoint = async (point: RecoveryPoint) => {
		const confirmation = window.prompt(`Type ${point.filename} to permanently delete this backup.`)
		if (confirmation === null) return
		setRecoveryBusy(`delete-${point.id}`)
		setRecoveryError('')
		try {
			await apiRequest<void>(token, `/api/v1/recovery-points/${point.id}`, { method: 'DELETE', body: JSON.stringify({ confirm_filename: confirmation }) })
			await loadRecoveryPoints()
		} catch (requestError) {
			setRecoveryError(requestError instanceof Error ? requestError.message : 'Backup could not be deleted')
		} finally {
			setRecoveryBusy('')
		}
	}

	const runHermesUpdate = async () => {
		if (instanceActionBusy || !hermesUpdate?.available || !hermesUpdate.eligible ||
			!['VERSION_UPDATE', 'RUNTIME_REFRESH'].includes(hermesUpdate.update_kind) ||
			!hermesUpdate.target_version || !canRunHermesMaintenance) return
		const targetVersion = hermesUpdate.target_version
		const runtimeRefresh = hermesUpdate.update_kind === 'RUNTIME_REFRESH'
		const recoveringFailedRuntime = runtimeRefresh && instance.status === 'FAILED'
		const wasRunning = instance.status === 'RUNNING' || recoveringFailedRuntime
		const workflowID = window.crypto.randomUUID()
		const confirmation = window.confirm(recoveringFailedRuntime
			? `Recover the managed runtime for ${instance.name}? Fleet verified that its retained artifacts are intact. Fleet will stop retained services, create and verify a rollback backup, refresh the Fleet-managed runtime, health-check Hermes, then restore the instance to RUNNING. Progress remains visible on this page.`
			: runtimeRefresh
			? `Run managed runtime maintenance for ${instance.name}? Hermes ${targetVersion} will remain installed. Fleet will stop the instance if needed, create and verify a rollback backup, refresh Fleet-managed runtime components, health-check Hermes, then ${wasRunning ? 'start the instance again' : 'leave the instance stopped'}. Progress remains visible on this page.`
			: `Update ${instance.name} from Hermes ${observation?.hermes_version ?? 'the installed version'} to ${targetVersion}? Fleet will stop the instance if needed, create and verify a rollback backup, install and health-check Hermes, then ${wasRunning ? 'start the instance again' : 'leave the instance stopped'}. Progress remains visible on this page.`,
		)
		if (!confirmation) return
		const controller = beginActionPoll('hermes-update')
		if (!controller) return

		setStartingHermesUpdate(true)
		setHermesUpdateStartError('')
		try {
			const update = await apiRequest<Operation>(token, `/api/v1/instances/${instance.id}/hermes-update`, {
				method: 'POST', body: JSON.stringify({
					confirm_name: instance.name,
					workflow_id: workflowID,
					restore_status: recoveringFailedRuntime ? 'RUNNING' : undefined,
				}),
				signal: controller.signal,
			})
			onOperation(update)
			setPendingHermesUpdate(update)
			await onChanged()
		} catch (requestError) {
			if (requestError instanceof DOMException && requestError.name === 'AbortError') return
			setHermesUpdateStartError(requestError instanceof Error ? requestError.message : `${runtimeRefresh ? 'Managed runtime maintenance' : 'Hermes update'} could not be queued`)
		} finally {
			finishActionPoll(controller)
			setStartingHermesUpdate(false)
		}
	}

	const retryProvisioning = async () => {
		if (instanceActionBusy || !failedProvisioningRetryAvailable) return
		const controller = beginActionPoll('retry-provisioning')
		if (!controller) return
		setObservationError('')
		try {
			let operation = await apiRequest<Operation>(token, `/api/v1/instances/${instance.id}/actions`, {
				method: 'POST',
				body: JSON.stringify({
					action: 'retry',
					workflow_id: window.crypto.randomUUID(),
				}),
				signal: controller.signal,
			})
			onOperation(operation)
			if (['PENDING', 'RUNNING'].includes(operation.status)) {
				operation = await waitForOperation(token, operation.id, controller.signal)
				onOperation(operation)
			}
			if (operation.status === 'FAILED') {
				throw new Error(operation.error || `${operation.summary} failed`)
			}
			await onChanged()
		} catch (requestError) {
			if (requestError instanceof DOMException && requestError.name === 'AbortError') return
			setObservationError(requestError instanceof Error ? requestError.message : 'Provisioning retry failed')
		} finally {
			finishActionPoll(controller)
		}
	}

  return (
    <div className="profile-layout">
      <nav className="instance-tabs" aria-label="Instance modules">
        {instanceTabs.map((tab) => <button key={tab.id} aria-current={selectedTab === tab.id ? 'page' : undefined} onClick={() => setSelectedTab(tab.id)}>{tab.label}{tab.id === 'overview' && operationalIssueChecks.length > 0 && <span className="tab-count">{operationalIssueChecks.length}</span>}{tab.id === 'operations' && activeInstanceOperationCount > 0 && <span className="tab-count" aria-hidden="true">{activeInstanceOperationCount}</span>}</button>)}
      </nav>

      {selectedTab === 'overview' && <div className="profile-tab-content">
        <div className="overview-grid">
          <section className="overview-card"><span>Runtime</span><div><Status value={instance.status} label={hermesUpdateFlow.status === 'running' ? updatePanelRuntimeRefresh ? 'Refreshing managed runtime' : 'Updating Hermes' : runtimeStatusLabel(instance.status)} /></div><small>{instance.host_name}</small></section>
	          <section className="overview-card"><span>Hermes version</span><strong title={effectiveInstalledVersion}>{effectiveInstalledVersion}</strong><small>{installedVersionVerified && observation?.received_at ? `Verified by Host Agent ${relativeTime(observation.received_at)}` : effectiveInstalledVersion === 'Detecting' ? 'Waiting for Host Agent observation' : 'Recorded version · verification pending'}</small><small>{hermesUpdateStatusLabel(hermesUpdate, hermesUpdateError)}</small>{hermesUpdate?.latest_release && hermesUpdate.official_checked_at && <a className="release-source" href={hermesUpdate.latest_release.url} target="_blank" rel="noreferrer">GitHub Releases · checked {relativeTime(hermesUpdate.official_checked_at)}<ExternalLink size={12} /></a>}</section>
          {instance.provider === 'openai-codex' ? <section className="overview-card"><span>Codex sign-in</span><div><Status value={codexConnected ? 'CONNECTED' : codexAuthCheck?.status ?? 'UNKNOWN'} label={codexConnected ? 'Signed in' : codexAuthCheck?.status === 'DRIFT' ? 'Sign in required' : 'Checking'} /></div><small>{codexSignInDetail(codexConnected, hasCodexConfiguration, codexConfigurationActive)}</small></section> : <section className="overview-card"><span>Provider</span><strong>{instance.provider}</strong><small>Managed by Hermes Fleet</small></section>}
        </div>
        {(versionUpdateAvailable || runtimeRefreshRequired || hermesUpdateFlow.status !== 'idle') && <section className={`hermes-update-panel ${hermesUpdateFlow.status}`}>
				<div className="hermes-update-summary"><RefreshCw size={18} className={hermesUpdateFlow.status === 'running' ? 'spin' : ''} /><div><strong>{updatePanelTitle}</strong><span>{hermesUpdateFlow.status === 'idle' ? failedRuntimeRecoveryAvailable ? `Hermes ${effectiveInstalledVersion} remains installed. Fleet verified the retained artifacts and can recover them with a rollback backup, refreshed managed runtime, and a health-checked return to RUNNING.` : failedRuntimeRecoveryNeedsVerification ? hermesUpdate?.reason : updatePanelRuntimeRefresh ? `Hermes ${effectiveInstalledVersion} remains installed. Fleet will create and verify a rollback backup, refresh the Fleet-managed runtime, restore its current state, and verify Hermes health.` : `Installed version: ${effectiveInstalledVersion}. Fleet will prepare the release, create and verify a fresh backup, update Hermes, and restore the current runtime state.` : hermesUpdateFlow.detail}</span></div>{hermesUpdateFlow.status === 'idle' && failedRuntimeRecoveryNeedsVerification ? <button className="primary-button compact-button" onClick={() => void requestObservation()} disabled={instanceActionBusy || !observationReady || refreshingObservation || Boolean(instance.observation_request)}><RefreshCw size={16} className={refreshingObservation ? 'spin' : ''} />{instance.observation_request ? 'Verification pending' : refreshingObservation ? 'Requesting verification' : 'Verify retained runtime'}</button> : hermesUpdateFlow.status === 'idle' || hermesUpdateFlow.status === 'error' ? <button className="primary-button compact-button" onClick={() => void runHermesUpdate()} disabled={instanceActionBusy || startingHermesUpdate || !canRunHermesMaintenance}><RefreshCw size={16} className={startingHermesUpdate ? 'spin' : ''} />{updatePanelActionLabel}</button> : <button className="secondary-button compact-button" disabled>{updatePanelRuntimeRefresh ? 'Refreshing' : 'Updating'}</button>}</div>
			{hermesUpdateFlow.status !== 'idle' && <HermesUpdateProgress flow={hermesUpdateFlow} />}
		</section>}
        {runtimeRemediation && <section className={`runtime-remediation-panel ${runtimeRemediation.status.toLowerCase()}`}>
          <div className="runtime-remediation-summary"><RefreshCw size={18} className={['READY', 'QUEUED', 'VERIFYING'].includes(runtimeRemediation.status) || instance.status === 'RESTARTING' ? 'spin' : ''} /><div><strong>{runtimeRecoveryTitle(runtimeRemediation.status)}</strong><span>{runtimeRecoveryDetail(runtimeRemediation)}</span>{runtimeRemediation.last_error && <small>{runtimeRemediation.last_error}</small>}</div><Status value={runtimeRemediation.status} label={runtimeRecoveryStatusLabel(runtimeRemediation.status)} /></div>
          <div className="runtime-remediation-footer"><div><strong>Attempt {runtimeRemediation.total_attempts} of {runtimeRemediation.max_attempts}</strong><span>Phase {runtimeRemediation.phase} of {runtimeRemediation.max_phases} · {runtimeRecoveryPhaseLabel(runtimeRemediation.phase)}</span></div>{automaticRuntimeRecoveryActive && <button className="secondary-button compact-button danger-button" onClick={() => void cancelAutomaticRuntimeRecovery()} disabled={instanceActionBusy || cancelingRuntimeRecovery || instance.status === 'RESTARTING'}>{cancelingRuntimeRecovery ? 'Stopping' : instance.status === 'RESTARTING' ? 'Attempt in progress' : 'Stop automatic recovery'}</button>}{!automaticRuntimeRecoveryActive && <button className="primary-button compact-button" onClick={() => void repairRuntime()} disabled={instanceActionBusy || !canRepairRuntime || repairingRuntime}><RefreshCw size={16} />Repair and verify</button>}</div>
        </section>}
		<section className={`attention-card ${operationalIssueChecks.length === 0 && !failedProvisioningRetryAvailable && !manualRuntimeRecoveryRequired ? 'attention-clear' : ''}`}>
		  <div className="attention-heading"><div><span>{manualRuntimeRecoveryRequired ? 'Recovery issue' : failedProvisioningRetryAvailable ? 'Provisioning issue' : operationalIssueChecks.length > 0 ? 'Health issue' : 'Health'}</span><strong>{manualRuntimeRecoveryRequired ? 'Runtime state requires manual recovery' : failedProvisioningRetryAvailable ? 'Provisioning stopped before the managed runtime was ready' : runtimeDrift ? 'Managed runtime needs repair' : imageDrift ? 'Container image update needs review' : effectiveOperationalSummary}</strong><small>{manualRuntimeRecoveryRequired ? instance.last_error : failedProvisioningRetryAvailable ? 'Retry uses the current supported Fleet runtime wrapper.' : observationChecks.length > 0 ? `${passedChecks.length} checks passed · ${observationTimestamp(observation?.received_at)}` : observationTimestamp(observation?.received_at)}</small></div><Status value={manualRuntimeRecoveryRequired || failedProvisioningRetryAvailable ? 'FAILED' : effectiveOperationalHealth} label={manualRuntimeRecoveryRequired ? 'Recovery required' : failedProvisioningRetryAvailable ? 'Needs action' : healthStatusLabel(effectiveOperationalHealth)} /></div>
		  {(operationalIssueChecks.length > 0 || failedProvisioningRetryAvailable || manualRuntimeRecoveryRequired) && <div className="attention-actions">{manualRuntimeRecoveryRequired && <button className="primary-button compact-button" onClick={() => setSelectedTab('operations')}>Review failed operation</button>}{failedProvisioningRetryAvailable && <button className="primary-button compact-button" onClick={() => void retryProvisioning()} disabled={instanceActionBusy}><RefreshCw size={16} className={activeAction === 'retry-provisioning' ? 'spin' : ''} />{activeAction === 'retry-provisioning' ? 'Retrying provisioning' : 'Retry provisioning'}</button>}{runtimeDrift && !runtimeRemediation && !manualRuntimeRecoveryRequired && <button className="primary-button compact-button" onClick={() => void repairRuntime()} disabled={instanceActionBusy || !canRepairRuntime || repairingRuntime || refreshingObservation}><RefreshCw size={16} className={repairingRuntime ? 'spin' : ''} />{repairingRuntime || instance.status === 'RESTARTING' ? 'Repairing and verifying' : 'Repair and verify'}</button>}{imageDrift && !manualRuntimeRecoveryRequired && <button className="primary-button compact-button" onClick={() => void fixImageDrift()} disabled={instanceActionBusy || !canFixImageDrift || fixingImage || refreshingObservation}><Wrench size={16} />{imageRepairing ? 'Fix in progress' : fixingImage ? 'Queuing fix' : 'Fix automatically'}</button>}{!manualRuntimeRecoveryRequired && <button className="secondary-button compact-button" onClick={() => setSelectedTab('diagnostics')}>Review diagnostics</button>}</div>}
		</section>
		{codexSetupIssue && <section className="setup-card"><div><span>Codex setup</span><strong>{effectiveCodexSetupTitle}</strong><small>This setup is separate from runtime health.</small></div><Status value="UNKNOWN" label="Setup incomplete" /><div className="attention-actions">{!codexConnected ? <button className="primary-button compact-button" onClick={() => setCodexAuthOpen(true)} disabled={instanceActionBusy || instance.status !== 'RUNNING'}><KeyRound size={16} />Authenticate Codex</button> : !hasCodexConfiguration ? <button className="primary-button compact-button" onClick={() => setSelectedTab('configuration')} disabled={instanceActionBusy || !['RUNNING', 'STOPPED'].includes(instance.status)}><Settings size={16} />Configure Codex</button> : canSyncRuntime && <button className="primary-button compact-button" onClick={() => void syncRuntimeConfiguration()} disabled={instanceActionBusy || syncingRuntime || refreshingObservation}><Wrench size={16} />{runtimeSyncInProgress ? 'Setup in progress' : syncingRuntime ? 'Queuing setup' : 'Complete setup'}</button>}</div></section>}
        {observationError && <div className="inline-error">{observationError}</div>}
        {hermesUpdateError && <div className="inline-error">{hermesUpdateError}</div>}
        {hermesUpdateStartError && <div className="inline-error">{hermesUpdateStartError}</div>}
      </div>}

      {selectedTab === 'access' && <div className="profile-tab-content">
        <section className="section-block first-section profile-section">
			<div className="section-heading"><div><h2>Instance access</h2><p>Dashboard destinations, API endpoint, and shared credentials</p></div><Status value={managedRoutePublished ? 'READY' : instancePublishedRoute?.provider_state === 'failed' || instancePublishedRoute?.provider_state === 'configuration_mismatch' ? 'FAILED' : 'STOPPED'} label={managedRouteRevalidating ? 'Published · Revalidating' : managedRoutePublished ? 'Published' : publicationBusy ? 'Publishing' : instancePublishedRoute?.provider_state === 'configuration_mismatch' ? 'Configuration mismatch' : instancePublishedRoute?.provider_state === 'failed' ? 'Failed' : 'Not published'} /></div>
			<div className="access-subsection">
				<div className="access-subsection-heading"><div><h3>Destinations</h3><p>Public and host-local addresses for this instance</p></div></div>
				<div className="detail-list">
					{effectivePublicDashboardURL ? <div className="detail-row"><span>Public dashboard</span><a className="detail-link" href={effectivePublicDashboardURL} target="_blank" rel="noreferrer">{effectivePublicDashboardURL}<ExternalLink size={14} /></a></div> : <DetailRow label="Public dashboard" value="Not published" />}
					<CopyableDetailRow label="Local dashboard" value={`http://127.0.0.1:${instance.dashboard_port}`} />
					<CopyableDetailRow label="Local API" value={`http://127.0.0.1:${instance.api_port}/v1`} />
				</div>
			</div>
			<div className="access-subsection">
				<div className="access-subsection-heading"><div><h3>Shared credentials</h3><p>Dashboard authentication and API key are revealed once here</p></div></div>
				<div className="detail-list">{credentials ? <><CredentialRow label="Username" value={credentials.dashboard_username} initiallyVisible /><CredentialRow label="Password" value={credentials.dashboard_password} /><CredentialRow label="API key" value={credentials.api_server_key} /></> : <div className="reveal-row"><span>Credentials{credentialOperation && <small>{credentialOperation.status === 'PENDING' ? 'Queued' : 'Host Agent is reading the encrypted values'}</small>}</span><button className="secondary-button" onClick={() => void revealCredentials()} disabled={revealing || !['RUNNING', 'STOPPED'].includes(instance.status)}><Eye size={17} />{revealing ? credentialOperation?.status === 'PENDING' ? 'Queued' : 'Reading' : 'Reveal credentials'}</button></div>}</div>
				{error && <div className="inline-error">{error}</div>}
				{credentials && <div className="credential-footer"><span>Expires {relativeTimeFuture(expiresAt)}</span><button className="text-button" onClick={() => { setCredentials(null); setExpiresAt('') }}>Hide now</button></div>}
			</div>
			<div className="access-subsection public-publishing-subsection">
				<div className="access-subsection-heading"><div><h3>Public publishing</h3><p>Cloudflare hostname and publication health</p></div></div>
				{remoteAccessConfiguration?.mode === 'managed_cloudflare' ? <form className="public-dashboard-form" onSubmit={(event) => { event.preventDefault(); void publishDashboard(effectivePublishingHostname) }}>
					<div className="publishing-hostname-summary"><div><span>Public hostname</span><code>{effectivePublishingHostname || 'Configure Instance publishing first'}</code><small>{publishedHostname ? 'Locked while published.' : 'Generated by Fleet from its namespace, instance name, and verified zone.'}</small></div></div>
					<details className="publishing-technical"><summary>Technical details</summary><div className="publication-health" aria-label="Publication health"><div><span>DNS</span><strong>{remoteResourceStateLabel(instancePublishedRoute?.dns_state)}</strong></div><div><span>Route</span><strong>{remoteResourceStateLabel(instancePublishedRoute?.route_state)}</strong></div><div><span>Endpoint</span><strong>{remoteRouteEndpointLabel(instancePublishedRoute?.endpoint_state)}</strong></div></div><div className="detail-list"><CopyableDetailRow label="Service URL" value={publicDashboardOrigin} /><DetailRow label="Route owner" value="Cloudflare" /></div></details>
					{publicationOperation?.progress?.steps && <ol className="publication-progress" aria-label={publicationOperation.summary}>{publicationOperation.progress.steps.map((step) => <li key={step.stage} className={`publication-step ${step.status}`}><span aria-hidden="true">{step.status === 'succeeded' ? '✓' : step.status === 'failed' ? '✕' : step.status === 'running' ? '●' : '○'}</span><div><strong>{publicationStageLabel(step.stage)}</strong>{step.detail && <small>{step.detail}</small>}</div></li>)}</ol>}
					{publicationOperation?.status === 'FAILED' && <div className="publication-failure"><strong>{publicationOperation.progress?.detail || publicationOperation.error || 'Dashboard publication failed'}</strong><div className="button-row">{publicationOperation.progress?.action_code === 'replace_api_token' && <a className="secondary-button" href="#system/remote-access">Replace API token</a>}<button type="submit" className="secondary-button" disabled={publicationBusy || !effectivePublishingHostname}>Retry</button></div></div>}
					<div className="section-footer"><span>Fleet changes only DNS and ingress resources recorded as Fleet-owned.</span><div className="button-row"><a className="secondary-button" href="#system/remote-access">Remote access settings</a>{managedRoutePublished ? <button type="button" className="secondary-button danger-button" onClick={() => void publishDashboard('')} disabled={publicationBusy}>Unpublish</button> : <button type="submit" className="primary-button" disabled={publicationBusy || !remoteAccessConfiguration.instance_publishing_configured || !effectivePublishingHostname}>{publicationBusy ? <RefreshCw className="spin" size={16} /> : <ShieldCheck size={16} />}{publicationBusy ? 'Publishing dashboard' : 'Publish dashboard'}</button>}</div></div>
				</form> : <div className="access-provider-note"><span>{remoteAccessConfiguration?.mode === 'existing_endpoints' ? 'The external provider owns this dashboard route.' : 'Connect Instance publishing before assigning a public hostname.'}</span><a className="secondary-button" href="#system/remote-access">Remote access settings</a></div>}
			</div>
			{remoteAccessError && <div className="inline-error">{remoteAccessError}</div>}
		</section>
      </div>}

      {selectedTab === 'configuration' && <div className="profile-tab-content">
        <section className="section-block first-section profile-section">
          {!codexConnected && !hasCodexConfiguration ? <>
            <div className="section-heading"><div><h2>Codex configuration</h2><p>Configuration and authentication are tracked separately</p></div><Status value="UNKNOWN" label="Not configured" /></div>
            <div className="detail-list">
              <DetailRow label="Model" value="Not configured" />
              <DetailRow label="Reasoning" value="Not configured" />
              <DetailRow label="Service tier" value="Not configured" />
            </div>
            <div className="backup-scope"><KeyRound size={18} /><div><strong>Codex is not connected</strong><span>Authenticate this instance before saving its model, reasoning level, and service tier.</span></div><button className="primary-button compact-button" onClick={() => setCodexAuthOpen(true)} disabled={instance.status !== 'RUNNING'}><KeyRound size={16} />Authenticate Codex</button></div>
          </> : !codexConnected ? <>
            <div className="section-heading"><div><h2>Codex configuration</h2><p>Configuration exists · authentication is separate</p></div><Status value="DRIFT" label="Sign in required" /></div>
            <div className="detail-list">
              <DetailRow label="Model" value={instance.model} mono />
              <DetailRow label="Reasoning" value={sentenceCase(instance.reasoning)} />
              <DetailRow label="Service tier" value={sentenceCase(instance.service_tier)} />
            </div>
            <div className="backup-scope"><KeyRound size={18} /><div><strong>Configuration saved, Codex not connected</strong><span>These values are already stored for this instance. Authenticate Codex to make the saved configuration usable.</span></div><button className="primary-button compact-button" onClick={() => setCodexAuthOpen(true)} disabled={instance.status !== 'RUNNING'}><KeyRound size={16} />Authenticate Codex</button></div>
          </> : <>
			<div className="section-heading"><div><h2>{codexConfigurationActive ? 'Codex configuration' : hasCodexConfiguration ? 'Review Codex configuration' : 'Configure Codex'}</h2><p>{codexConfigurationActive ? 'Saved settings are applied to this instance' : hasCodexConfiguration ? 'Saved settings need to be applied to this instance' : 'Choose settings for this instance'}</p></div><Status value="CONNECTED" label="Signed in" /></div>
            <form className="codex-configuration-form" onSubmit={(event) => void configureCodex(event)}>
              <div className="form-grid">
				<label>Model<select value={codexForm.model} onChange={(event) => updateCodexForm('model', event.target.value)} required disabled={instanceActionBusy || configuringCodex || codexModelOptions.length === 0}><option value="">Select model</option>{codexModelOptions.map((model) => <option key={model} value={model}>{model}{model === recommendedCodexModel ? ' · recommended' : ''}</option>)}</select></label>
                <label>Reasoning<select value={codexForm.reasoning} onChange={(event) => updateCodexForm('reasoning', event.target.value)} required disabled={instanceActionBusy || configuringCodex}><option value="">Select reasoning</option><option value="low">Low</option><option value="medium">Medium</option><option value="high">High</option><option value="xhigh">Xhigh</option></select></label>
                <label>Service tier<select value={codexForm.service_tier} onChange={(event) => updateCodexForm('service_tier', event.target.value)} required disabled={instanceActionBusy || configuringCodex}><option value="">Select service tier</option><option value="normal">Normal</option><option value="priority">Priority</option></select></label>
              </div>
			  {codexModelOptions.length === 0 && <div className="inline-error">Hermes model catalog is unavailable. Refresh diagnostics before configuring Codex.</div>}
              {codexConfigurationError && <div className="inline-error">{codexConfigurationError}</div>}
              <div className="modal-actions"><button className="primary-button" type="submit" disabled={instanceActionBusy || configuringCodex || instance.status === 'UPDATING' || !codexDirty || !codexFormValid}><Settings size={16} />{configuringCodex ? 'Applying' : hasCodexConfiguration ? 'Save changes' : 'Save and apply'}</button></div>
            </form>
          </>}
        </section>
      </div>}

	  {selectedTab === 'profiles' && <HermesProfilesPanel instance={instance} token={token} onOperation={onOperation} refreshSignal={refreshSignal} blocked={instanceActionBusy} />}

      {selectedTab === 'messaging' && <MessagingSettings instance={instance} token={token} onChanged={onChanged} onOperation={onOperation} refreshSignal={refreshSignal} blocked={instanceActionBusy} />}

	  {selectedTab === 'mcp' && <MCPSettings instance={instance} token={token} onChanged={onChanged} onOperation={onOperation} refreshSignal={refreshSignal} blocked={instanceActionBusy} />}

      {selectedTab === 'recovery' && <div className="profile-tab-content">
        <section className="section-block first-section profile-section recovery-section">
		  <div className="section-heading"><div><h2>Backups</h2><p>Create and restore backups for {instance.name}</p></div><div className="section-actions">{instance.status === 'RUNNING' ? <button className="secondary-button" onClick={() => void stopForRecovery()} disabled={instanceActionBusy || recoveryBusy !== ''}><CircleStop size={16} />{recoveryBusy === 'stop' ? 'Stopping' : 'Stop instance'}</button> : <button className="primary-button" onClick={() => void createRecoveryPoint()} disabled={instanceActionBusy || instance.status !== 'STOPPED' || recoveryBusy !== ''}><Archive size={16} />{recoveryBusy === 'create' ? 'Creating' : 'Create backup'}</button>}</div></div>
		  <div className="backup-scope"><ShieldCheck size={18} /><div><strong>{instance.status === 'STOPPED' ? 'Create the backup here' : 'Stop the instance before creating a backup'}</strong><span>This page creates an encrypted copy of this instance's managed workspace, data volume, and Codex sign-in state. It records the Hermes release identity but does not copy Docker image layers. Backups listed below can be restored from this page while the instance is stopped.</span></div></div>
          {recoveryError && <div className="inline-error">{recoveryError}</div>}
		  {recoveryPoints.length === 0 ? <div className="compact-empty"><Archive size={18} /><div><strong>No backups yet</strong><span>{instance.status === 'STOPPED' ? 'Create the first backup above.' : 'Stop the instance above, then create the first backup.'}</span></div></div> : <div className="table-wrap"><table className="provider-table recovery-table"><thead><tr><th>Backup</th><th>Size</th><th>Status</th><th>Last verified</th><th><span className="sr-only">Actions</span></th></tr></thead><tbody>{recoveryPoints.map((point) => <tr key={point.id}><td data-label="Backup"><strong>{point.filename}</strong><span className="secondary-text" title={point.sha256}>{point.sha256 ? `SHA-256 ${point.sha256.slice(0, 16)}…` : point.error || 'Encrypted backup is being prepared'} · {relativeTime(point.created_at)}</span></td><td data-label="Size">{point.size_bytes > 0 ? formatBytes(point.size_bytes) : 'Pending'}</td><td data-label="Status"><Status value={point.status} /></td><td data-label="Last verified">{point.verified_at ? relativeTime(point.verified_at) : 'Pending'}</td><td data-label="Actions"><div className="row-actions"><button className="icon-button" title="Restore this backup" onClick={() => void restoreRecoveryPoint(point)} disabled={instanceActionBusy || point.status !== 'READY' || instance.status !== 'STOPPED' || recoveryBusy !== ''}><History size={15} /></button><button className="icon-button" title="Verify backup" onClick={() => void verifyRecoveryPoint(point)} disabled={instanceActionBusy || point.status !== 'READY' || recoveryBusy !== ''}><ShieldCheck size={15} /></button><button className="icon-button" title="Download backup" onClick={() => void downloadRecoveryPoint(point)} disabled={point.status !== 'READY' || recoveryBusy !== ''}><Download size={15} /></button><button className="icon-button danger-button" title="Delete backup" onClick={() => void deleteRecoveryPoint(point)} disabled={instanceActionBusy || !['READY', 'FAILED'].includes(point.status) || recoveryBusy !== ''}><Trash2 size={15} /></button></div></td></tr>)}</tbody></table></div>}
        </section>
      </div>}

      {selectedTab === 'diagnostics' && <div className="profile-tab-content">
        <section className="section-block first-section profile-section observation-section">
		  <div className="section-heading"><div><h2>Diagnostics</h2><p>{operationalIssueChecks.length} {plural(operationalIssueChecks.length, 'issue')}{setupItems.length > 0 ? ` · ${setupItems.length} setup ${plural(setupItems.length, 'item')}` : ''} · {passedChecks.length} passed</p></div><button className="secondary-button compact-button" onClick={() => void requestObservation()} disabled={instanceActionBusy || !observationReady || refreshingObservation || fixingImage || Boolean(instance.observation_request)}><RefreshCw size={16} className={refreshingObservation ? 'spin' : ''} />{instance.observation_request ? 'Refresh pending' : refreshingObservation ? 'Requesting' : 'Refresh diagnostics'}</button></div>
          {observationError && <div className="inline-error">{observationError}</div>}
          {runtimeRefreshRequired && hermesUpdateFlow.status === 'idle' && <div className="repair-callout"><RefreshCw size={18} /><div><strong>{failedRuntimeRecoveryAvailable ? 'Managed runtime recovery' : failedRuntimeRecoveryNeedsVerification ? 'Verify retained runtime' : 'Managed runtime maintenance'}</strong><span>{failedRuntimeRecoveryAvailable ? `Hermes ${hermesUpdate?.current_version} remains installed. Fleet verified the retained artifacts and can recover the instance to RUNNING.` : failedRuntimeRecoveryNeedsVerification ? hermesUpdate?.reason : `Hermes ${hermesUpdate?.current_version} remains installed. Fleet will create and verify a rollback backup, refresh its managed runtime components, restore the current state, and verify Hermes health.`}</span></div>{failedRuntimeRecoveryNeedsVerification ? <button className="primary-button compact-button" onClick={() => void requestObservation()} disabled={instanceActionBusy || !observationReady || refreshingObservation || Boolean(instance.observation_request)}><RefreshCw size={16} className={refreshingObservation ? 'spin' : ''} />Verify runtime</button> : <button className="primary-button compact-button" onClick={() => void runHermesUpdate()} disabled={instanceActionBusy || startingHermesUpdate || !canRunHermesMaintenance}><RefreshCw size={16} className={startingHermesUpdate ? 'spin' : ''} />{startingHermesUpdate ? 'Queuing maintenance' : failedRuntimeRecoveryAvailable ? 'Recover runtime' : 'Run maintenance'}</button>}</div>}
          {runtimeDrift && <div className="repair-callout"><RefreshCw size={18} className={repairingRuntime || automaticRuntimeRecoveryActive ? 'spin' : ''} /><div><strong>{runtimeRemediation ? runtimeRecoveryTitle(runtimeRemediation.status) : repairingRuntime || instance.status === 'RESTARTING' ? 'Runtime repair and health verification in progress' : 'Managed repair available'}</strong><span>{runtimeRemediation ? runtimeRecoveryDetail(runtimeRemediation) : 'Fleet will repair the managed services, preserve instance data and configuration, then verify Hermes and dashboard health before reporting success.'}</span></div>{runtimeRemediation && automaticRuntimeRecoveryActive ? <button className="secondary-button compact-button danger-button" onClick={() => void cancelAutomaticRuntimeRecovery()} disabled={instanceActionBusy || cancelingRuntimeRecovery || instance.status === 'RESTARTING'}>{instance.status === 'RESTARTING' ? 'Attempt in progress' : cancelingRuntimeRecovery ? 'Stopping' : 'Stop automatic recovery'}</button> : <button className="primary-button compact-button" onClick={() => void repairRuntime()} disabled={instanceActionBusy || !canRepairRuntime || repairingRuntime}>{repairingRuntime || instance.status === 'RESTARTING' ? 'Repairing' : 'Repair and verify'}</button>}</div>}
          {imageDrift && <div className="repair-callout"><Wrench size={18} /><div><strong>{imageRepairing ? 'Automatic fix in progress' : 'Automatic fix available'}</strong><span>{instance.status === 'STOPPED' ? 'Fleet will verify ownership and the current image without starting this instance.' : 'Fleet will run safety checks, restart this instance briefly, and confirm Hermes is healthy.'}</span></div><button className="primary-button compact-button" onClick={() => void fixImageDrift()} disabled={instanceActionBusy || !canFixImageDrift || fixingImage}>{imageRepairing ? 'Fixing' : fixingImage ? 'Queuing' : 'Fix automatically'}</button></div>}
		  {codexSetupIssue && !runtimeRefreshRequired && <div className="repair-callout"><Wrench size={18} /><div><strong>{codexSetupTitle}</strong><span>{codexSetupIssue.detail}</span></div>{!codexConnected ? <button className="primary-button compact-button" onClick={() => setCodexAuthOpen(true)} disabled={instanceActionBusy || instance.status !== 'RUNNING'}><KeyRound size={16} />Authenticate Codex</button> : !hasCodexConfiguration ? <button className="primary-button compact-button" onClick={() => setSelectedTab('configuration')} disabled={instanceActionBusy || !['RUNNING', 'STOPPED'].includes(instance.status)}><Settings size={16} />Configure Codex</button> : <button className="primary-button compact-button" onClick={() => void syncRuntimeConfiguration()} disabled={instanceActionBusy || !canSyncRuntime || syncingRuntime}>{runtimeSyncInProgress ? 'Applying' : syncingRuntime ? 'Queuing' : 'Complete setup'}</button>}</div>}
          {observationChecks.length === 0 ? <div className="observation-empty">No diagnostics have been received.</div> : visibleDiagnostics.length > 0 ? <div className="table-wrap"><table className="provider-table observation-table"><thead><tr><th>Check</th><th>Result</th><th>Detail</th></tr></thead><tbody>{visibleDiagnostics.map((check) => <tr key={check.name}><td data-label="Check"><strong>{observationCheckLabel(check.name)}</strong></td><td data-label="Result">{check.name === 'codex_setup' ? <Status value="UNKNOWN" label="Setup incomplete" /> : <Status value={check.status} />}</td><td data-label="Detail">{check.detail}</td></tr>)}</tbody></table></div> : <div className="compact-empty"><ShieldCheck size={18} /><div><strong>No issues found</strong><span>All {passedChecks.length} diagnostics passed.</span></div></div>}
          {passedChecks.length > 0 && <div className="diagnostic-disclosure"><button className="text-button" onClick={() => setShowPassedDiagnostics((current) => !current)} aria-expanded={showPassedDiagnostics}>{showPassedDiagnostics ? 'Hide passed checks' : `Show ${passedChecks.length} passed ${plural(passedChecks.length, 'check')}`}</button></div>}
        </section>
      </div>}

      {selectedTab === 'operations' && <div className="profile-tab-content">
		<OperationsWorkspace operations={updateOperations} instances={[instance]} token={token} fixedInstanceID={instance.id} pageSize={5} onChanged={onChanged} />
      </div>}

      {codexAuthOpen && <CodexAuthDialog instance={instance} token={token} onClose={() => setCodexAuthOpen(false)} onConnected={() => { void requestObservation(); onChanged() }} />}
    </div>
  )
}

function HermesProfilesPanel({
	instance,
	token,
	onOperation,
	refreshSignal,
	blocked = false,
}: {
	instance: Instance
	token: string
	onOperation: (operation: Operation) => void
	refreshSignal: number
	blocked?: boolean
}) {
	const [inventory, setInventory] = useState<HermesProfileInventory | null>(null)
	const [loading, setLoading] = useState(true)
	const [busy, setBusy] = useState('')
	const [error, setError] = useState('')
	const [notice, setNotice] = useState('')
	const [showCreate, setShowCreate] = useState(false)
	const [profileName, setProfileName] = useState('')
	const [cloneFrom, setCloneFrom] = useState('')
	const [description, setDescription] = useState('')
	const loadController = useRef<AbortController | null>(null)
	const autoSyncInstance = useRef('')

	const loadProfiles = useCallback(async () => {
		loadController.current?.abort()
		const controller = new AbortController()
		loadController.current = controller
		setLoading(true)
		try {
			const next = await apiRequest<HermesProfileInventory>(token, `/api/v1/instances/${instance.id}/profiles`, {
				cache: 'no-store',
				signal: controller.signal,
			})
			setInventory(next)
			setCloneFrom((current) => current || next.profiles.find((profile) => profile.default)?.name || next.profiles[0]?.name || '')
			setError('')
		} catch (requestError) {
			if (!(requestError instanceof DOMException && requestError.name === 'AbortError')) {
				setError(requestError instanceof Error ? requestError.message : 'Hermes profiles could not be loaded')
			}
		} finally {
			if (!controller.signal.aborted) setLoading(false)
		}
	}, [instance.id, token])

	useEffect(() => {
		const scheduledLoad = window.setTimeout(() => void loadProfiles(), 0)
		return () => {
			window.clearTimeout(scheduledLoad)
			loadController.current?.abort()
		}
	}, [loadProfiles, refreshSignal])

	const executeProfileOperation = useCallback(async (path: string, options: RequestInit, signal: AbortSignal) => {
		let operation = await apiRequest<Operation>(token, path, { ...options, signal })
		onOperation(operation)
		if (['PENDING', 'RUNNING'].includes(operation.status)) {
			operation = await waitForOperation(token, operation.id, signal)
			onOperation(operation)
		}
		return operation
	}, [onOperation, token])

	const runOperation = async (action: string, path: string, options: RequestInit = {}) => {
		const controller = new AbortController()
		setBusy(action)
		setError('')
		setNotice('')
		try {
			await executeProfileOperation(path, options, controller.signal)
			await loadProfiles()
			return true
		} catch (requestError) {
			setError(requestError instanceof Error ? requestError.message : 'Hermes profile operation failed')
			return false
		} finally {
			setBusy('')
		}
	}

	const syncProfiles = useCallback(async (announce = true) => {
		if (instance.status !== 'RUNNING' || blocked) return
		const controller = new AbortController()
		setBusy('sync')
		setError('')
		setNotice('')
		let repaired = false
		try {
			try {
				await executeProfileOperation(`/api/v1/instances/${instance.id}/profiles/refresh`, { method: 'POST' }, controller.signal)
			} catch (requestError) {
				if (!isRepairableHermesProfileAccessError(requestError)) throw requestError
				repaired = true
				await executeProfileOperation(`/api/v1/instances/${instance.id}/profiles/repair`, { method: 'POST' }, controller.signal)
			}
			await loadProfiles()
			if (announce) setNotice(repaired ? 'Profile access repaired and profiles synced.' : 'Profiles synced.')
		} catch (requestError) {
			setError(requestError instanceof Error ? requestError.message : 'Hermes profiles could not be synced')
		} finally {
			setBusy('')
		}
	}, [blocked, executeProfileOperation, instance.id, instance.status, loadProfiles])

	const createProfile = async (event: FormEvent) => {
		event.preventDefault()
		const created = await runOperation('create', `/api/v1/instances/${instance.id}/profiles`, {
			method: 'POST',
			body: JSON.stringify({ name: profileName.trim(), clone_from: cloneFrom, description: description.trim() }),
		})
		if (created) {
			setProfileName('')
			setDescription('')
			setShowCreate(false)
		}
	}

	const activateProfile = async (name: string) => {
		const activated = await runOperation(`activate:${name}`, `/api/v1/instances/${instance.id}/profiles/${encodeURIComponent(name)}/active`, {
			method: 'POST',
		})
		if (activated) setNotice(`${name} is now the active profile for new Hermes commands.`)
	}

	const deleteProfile = async (name: string) => {
		if (!window.confirm(`Delete Hermes profile ${name}? Its configuration, API keys, memory, sessions, skills, and cron jobs will be permanently removed.`)) return
		const deleted = await runOperation(`delete:${name}`, `/api/v1/instances/${instance.id}/profiles/${encodeURIComponent(name)}`, {
			method: 'DELETE',
		})
		if (deleted) setNotice(`${name} was deleted.`)
	}

	const profiles = inventory?.profiles ?? []
	const available = instance.status === 'RUNNING' && !blocked && busy === ''
	const inventoryObservedAt = inventory?.observed_at && !inventory.observed_at.startsWith('0001-')
		? inventory.observed_at
		: ''

	useEffect(() => {
		if (loading || !available || autoSyncInstance.current === instance.id) return
		const observedAt = inventoryObservedAt ? Date.parse(inventoryObservedAt) : 0
		const stale = !observedAt || Date.now() - observedAt > 5 * 60 * 1000
		if (!stale) return
		const scheduledSync = window.setTimeout(() => {
			autoSyncInstance.current = instance.id
			void syncProfiles(false)
		}, 0)
		return () => window.clearTimeout(scheduledSync)
	}, [available, instance.id, inventoryObservedAt, loading, syncProfiles])

	return <div className="profile-tab-content">
		<section className="section-block first-section profile-section hermes-profiles-section">
			<div className="section-heading">
				<div><h2>Hermes profiles</h2><p>Authoritative profiles reported by {instance.name}; secrets and filesystem paths stay on the managed host</p></div>
				<div className="section-actions">
					<button className="secondary-button compact-button" onClick={() => void syncProfiles()} disabled={!available}>
						<RefreshCw size={16} className={busy === 'sync' ? 'spin' : ''} />{busy === 'sync' ? 'Syncing profiles' : 'Sync profiles'}
					</button>
					<button className="primary-button compact-button" onClick={() => setShowCreate((current) => !current)} disabled={!available || profiles.length === 0} aria-expanded={showCreate}>
						<Plus size={16} />Create from profile
					</button>
				</div>
			</div>
			<div className="backup-scope"><ShieldCheck size={18} /><div><strong>Fleet-owned transport boundary</strong><span>Fleet requests inventory and lifecycle operations through the owning Host Agent. Active selects the profile for future Hermes commands; Gateway reports whether its process is running. The browser never receives Hermes profile tokens.</span></div></div>
			{showCreate && <form className="hermes-profile-create" onSubmit={(event) => void createProfile(event)}>
				<div className="form-grid">
					<label>Profile name<input value={profileName} onChange={(event) => {
						const value = event.target.value.toLowerCase()
						event.currentTarget.setCustomValidity(hermesReservedProfileNames.has(value) ? 'This profile name is reserved by Hermes.' : '')
						setProfileName(value)
					}} pattern="[a-z0-9][a-z0-9_-]{0,63}" maxLength={64} placeholder="research-worker" required disabled={!available} /></label>
					<label>Clone from<select value={cloneFrom} onChange={(event) => setCloneFrom(event.target.value)} required disabled={!available}>{profiles.map((profile) => <option key={profile.name} value={profile.name}>{profile.name}{profile.default ? ' · default' : ''}</option>)}</select></label>
					<label className="form-wide">Description<input value={description} onChange={(event) => setDescription(event.target.value)} maxLength={1000} placeholder="Dedicated profile purpose" disabled={!available} /></label>
				</div>
					<div className="modal-actions"><button type="button" className="secondary-button" onClick={() => setShowCreate(false)} disabled={busy === 'create'}>Cancel</button><button type="submit" className="primary-button" disabled={!available || !profileName.trim() || hermesReservedProfileNames.has(profileName.trim()) || !cloneFrom || profileName.trim() === cloneFrom}>{busy === 'create' ? <RefreshCw className="spin" size={16} /> : <Plus size={16} />}{busy === 'create' ? 'Creating profile' : 'Create profile'}</button></div>
			</form>}
			{notice && <div className="inline-notice">{notice}</div>}
			{error && <div className="inline-error">{error}</div>}
			{loading ? <div className="compact-empty"><LoaderCircle className="spin" size={18} /><div><strong>Loading profiles</strong><span>Reading the latest Fleet snapshot.</span></div></div> : profiles.length === 0 ? <div className="compact-empty"><Bot size={18} /><div><strong>No profile inventory yet</strong><span>Use Sync profiles to read the authoritative Hermes inventory. Fleet repairs legacy profile access only when refresh reports an access failure.</span></div></div> : <div className="table-wrap"><table className="provider-table hermes-profiles-table"><thead><tr><th>Profile</th><th>Runtime</th><th>Provider / model</th><th><span className="sr-only">Actions</span></th></tr></thead><tbody>{profiles.map((profile) => <tr key={profile.name}>
				<td data-label="Profile"><strong>{profile.name}</strong><span className="secondary-text">{profile.description || 'No description'}</span></td>
				<td data-label="Runtime"><div className="profile-statuses">{profile.default && <Status value="READY" label="Default" />}{profile.active && <Status value="RUNNING" label="Active" />}{profile.gateway_running ? <Status value="ONLINE" label="Gateway" /> : <Status value="STOPPED" label="Gateway stopped" />}</div></td>
				<td data-label="Provider / model"><strong>{profile.provider || 'Not reported'}</strong><span className="secondary-text">{profile.model || 'Model not reported'}</span></td>
				<td data-label="Actions"><div className="row-actions"><button className="icon-button" title={profile.active ? `${profile.name} is already active` : `Set ${profile.name} as active`} onClick={() => void activateProfile(profile.name)} disabled={!available || profile.active}>{busy === `activate:${profile.name}` ? <RefreshCw className="spin" size={15} /> : <Check size={15} />}</button>{!profile.default && <button className="icon-button danger-button" title={`Delete ${profile.name}`} onClick={() => void deleteProfile(profile.name)} disabled={!available}>{busy === `delete:${profile.name}` ? <RefreshCw className="spin" size={15} /> : <Trash2 size={15} />}</button>}</div></td>
			</tr>)}</tbody></table></div>}
			<div className="section-footer"><span>{inventoryObservedAt ? `Observed ${relativeTime(inventoryObservedAt)}` : 'Inventory has not been observed yet.'}</span><span>{profiles.length} {plural(profiles.length, 'profile')}</span></div>
		</section>
	</div>
}

type MessagingFormState = {
	telegramEnabled: boolean
	telegramAllowedUsers: string
	telegramGroupAllowedUsers: string
	telegramGroupAllowedChats: string
	telegramRequireMention: boolean
	telegramProxyURL: string
	whatsAppEnabled: boolean
	whatsAppMode: 'bot' | 'self-chat'
	whatsAppAllowedUsers: string
	whatsAppUnauthorizedDMBehavior: 'ignore' | 'pair'
	whatsAppReplyPrefix: string
}

const emptyMessagingForm: MessagingFormState = {
	telegramEnabled: false,
	telegramAllowedUsers: '',
	telegramGroupAllowedUsers: '',
	telegramGroupAllowedChats: '',
	telegramRequireMention: true,
	telegramProxyURL: '',
	whatsAppEnabled: false,
	whatsAppMode: 'bot',
	whatsAppAllowedUsers: '',
	whatsAppUnauthorizedDMBehavior: 'ignore',
	whatsAppReplyPrefix: '⚕ **Hermes Agent**',
}

function normalizeMessagingConfiguration(value: unknown): MessagingConfiguration {
	const root = value && typeof value === 'object' ? value as Record<string, unknown> : {}
	const telegram = root.telegram && typeof root.telegram === 'object' ? root.telegram as Record<string, unknown> : {}
	const whatsApp = root.whatsapp && typeof root.whatsapp === 'object' ? root.whatsapp as Record<string, unknown> : {}
	const statuses: MessagingConfiguration['status'][] = ['NOT_CONFIGURED', 'PENDING', 'APPLIED', 'FAILED']
	const status = statuses.includes(root.status as MessagingConfiguration['status'])
		? root.status as MessagingConfiguration['status']
		: 'NOT_CONFIGURED'
	const mode = whatsApp.mode === 'self-chat' ? 'self-chat' : 'bot'
	const unauthorizedDMBehavior = whatsApp.unauthorized_dm_behavior === 'pair' ? 'pair' : 'ignore'
	const list = (candidate: unknown) => Array.isArray(candidate)
		? candidate.filter((item): item is string => typeof item === 'string')
		: []
	const optionalString = (candidate: unknown) => typeof candidate === 'string' && candidate ? candidate : undefined

	return {
		status,
		...(optionalString(root.last_error) ? { last_error: optionalString(root.last_error) } : {}),
		...(optionalString(root.desired_revision) ? { desired_revision: optionalString(root.desired_revision) } : {}),
		...(optionalString(root.applied_revision) ? { applied_revision: optionalString(root.applied_revision) } : {}),
		...(optionalString(root.updated_at) ? { updated_at: optionalString(root.updated_at) } : {}),
		...(optionalString(root.applied_at) ? { applied_at: optionalString(root.applied_at) } : {}),
		telegram: {
			enabled: telegram.enabled === true,
			token_configured: telegram.token_configured === true,
			token_hint: typeof telegram.token_hint === 'string' ? telegram.token_hint : '',
			allowed_users: list(telegram.allowed_users),
			group_allowed_users: list(telegram.group_allowed_users),
			group_allowed_chats: list(telegram.group_allowed_chats),
			require_mention: telegram.require_mention !== false,
			proxy_url: typeof telegram.proxy_url === 'string' ? telegram.proxy_url : '',
		},
		whatsapp: {
			enabled: whatsApp.enabled === true,
			mode,
			allowed_users: list(whatsApp.allowed_users),
			unauthorized_dm_behavior: unauthorizedDMBehavior,
			reply_prefix: typeof whatsApp.reply_prefix === 'string' ? whatsApp.reply_prefix : '⚕ **Hermes Agent**',
		},
	}
}

function messagingFormFromConfiguration(value: MessagingConfiguration): MessagingFormState {
	return {
		telegramEnabled: value.telegram.enabled,
		telegramAllowedUsers: renderManagedList(value.telegram.allowed_users),
		telegramGroupAllowedUsers: renderManagedList(value.telegram.group_allowed_users),
		telegramGroupAllowedChats: renderManagedList(value.telegram.group_allowed_chats),
		telegramRequireMention: value.telegram.require_mention,
		telegramProxyURL: value.telegram.proxy_url,
		whatsAppEnabled: value.whatsapp.enabled,
		whatsAppMode: value.whatsapp.mode,
		whatsAppAllowedUsers: renderManagedList(value.whatsapp.allowed_users),
		whatsAppUnauthorizedDMBehavior: value.whatsapp.unauthorized_dm_behavior,
		whatsAppReplyPrefix: value.whatsapp.reply_prefix,
	}
}

function messagingFormsEqual(left: MessagingFormState, right: MessagingFormState) {
	return JSON.stringify(left) === JSON.stringify(right)
}

function messagingConfigurationFingerprint(value: MessagingConfiguration) {
	return value.desired_revision || JSON.stringify({
		telegram: value.telegram,
		whatsapp: value.whatsapp,
	})
}

function validateMessagingForm(
	form: MessagingFormState,
	telegramBotToken: string,
	clearTelegramToken: boolean,
	tokenConfigured: boolean,
) {
	const telegramAllowedUsers = parseManagedList(form.telegramAllowedUsers)
	const telegramGroupAllowedUsers = parseManagedList(form.telegramGroupAllowedUsers)
	const telegramGroupAllowedChats = parseManagedList(form.telegramGroupAllowedChats)
	const telegramIDs = [...telegramAllowedUsers, ...telegramGroupAllowedUsers, ...telegramGroupAllowedChats]
	if (telegramIDs.some((value) => !/^-?[0-9]{1,20}$/.test(value))) {
		return 'Telegram user and chat IDs must be numeric and at most 20 digits.'
	}
	if (form.telegramEnabled && !telegramBotToken.trim() && (!tokenConfigured || clearTelegramToken)) {
		return 'Enter a Telegram bot token before enabling Telegram.'
	}
	if (form.telegramEnabled && telegramAllowedUsers.length === 0) {
		return 'Allow at least one Telegram user before enabling Telegram.'
	}
	const proxyURL = form.telegramProxyURL.trim()
	if (proxyURL) {
		try {
			const proxy = new URL(proxyURL)
			if (!['http:', 'https:', 'socks5:'].includes(proxy.protocol) || !proxy.host) {
				return 'Telegram proxy must be an http, https, or socks5 URL.'
			}
			if (proxy.username || proxy.password) return 'Telegram proxy credentials cannot be stored in the URL.'
		} catch {
			return 'Telegram proxy must be an http, https, or socks5 URL.'
		}
	}
	const whatsAppNumbers = parseManagedList(form.whatsAppAllowedUsers)
	if (whatsAppNumbers.some((value) => !/^[1-9][0-9]{6,14}$/.test(value))) {
		return 'WhatsApp numbers must use international format without +.'
	}
	if (form.whatsAppEnabled && form.whatsAppMode === 'bot' && whatsAppNumbers.length === 0) {
		return 'Allow at least one WhatsApp number before enabling bot mode.'
	}
	if (new TextEncoder().encode(form.whatsAppReplyPrefix.trim()).length > 240) {
		return 'WhatsApp reply prefix must be at most 240 bytes.'
	}
	return ''
}

function MessagingSettings({
	instance,
	token,
	onChanged,
	onOperation,
	refreshSignal,
	blocked = false,
}: {
	instance: Instance
	token: string
	onChanged: () => Promise<void>
	onOperation: (operation: Operation) => void
	refreshSignal: number
	blocked?: boolean
}) {
	const [configuration, setConfiguration] = useState<MessagingConfiguration | null>(null)
	const [form, setForm] = useState<MessagingFormState>(emptyMessagingForm)
	const [savedForm, setSavedForm] = useState<MessagingFormState>(emptyMessagingForm)
	const [telegramBotToken, setTelegramBotToken] = useState('')
	const [replacingTelegramToken, setReplacingTelegramToken] = useState(false)
	const [clearTelegramToken, setClearTelegramToken] = useState(false)
	const [loading, setLoading] = useState(true)
	const [saving, setSaving] = useState(false)
	const [error, setError] = useState('')
	const [stale, setStale] = useState(false)
	const loadController = useRef<AbortController | null>(null)
	const loadSequence = useRef(0)
	const lastRefreshSignal = useRef(refreshSignal)
	const configurationFingerprint = useRef('')
	const dirtyRef = useRef(false)
	const dirty = !messagingFormsEqual(form, savedForm) || telegramBotToken.trim() !== '' || clearTelegramToken
	const validationError = validateMessagingForm(
		form,
		telegramBotToken,
		clearTelegramToken,
		configuration?.telegram.token_configured ?? false,
	)

	useEffect(() => {
		dirtyRef.current = dirty
	}, [dirty])

	const hydrateForm = useCallback((value: MessagingConfiguration) => {
		const nextForm = messagingFormFromConfiguration(value)
		setForm(nextForm)
		setSavedForm(nextForm)
		setTelegramBotToken('')
		setReplacingTelegramToken(false)
		setClearTelegramToken(false)
		setStale(false)
		configurationFingerprint.current = messagingConfigurationFingerprint(value)
		dirtyRef.current = false
	}, [])

	const loadConfiguration = useCallback(async (replaceForm: boolean) => {
		const sequence = loadSequence.current + 1
		loadSequence.current = sequence
		loadController.current?.abort()
		const controller = new AbortController()
		loadController.current = controller
		try {
			const payload = await apiRequest<unknown>(token, `/api/v1/instances/${instance.id}/messaging`, {
				cache: 'no-store',
				signal: controller.signal,
			})
			if (controller.signal.aborted || sequence !== loadSequence.current) return null
			const value = normalizeMessagingConfiguration(payload)
			setConfiguration(value)
			if (replaceForm || !dirtyRef.current) {
				hydrateForm(value)
			} else if (messagingConfigurationFingerprint(value) !== configurationFingerprint.current) {
				setStale(true)
			}
			setError('')
			return value
		} catch (requestError) {
			if (requestError instanceof DOMException && requestError.name === 'AbortError') return null
			if (sequence !== loadSequence.current) return null
			setError(requestError instanceof Error ? requestError.message : 'Messaging configuration could not be loaded')
			return null
		} finally {
			if (sequence === loadSequence.current) setLoading(false)
			if (loadController.current === controller) loadController.current = null
		}
	}, [hydrateForm, instance.id, token])

	useEffect(() => {
		const initial = window.setTimeout(() => {
			setLoading(true)
			setConfiguration(null)
			setForm(emptyMessagingForm)
			setSavedForm(emptyMessagingForm)
			setTelegramBotToken('')
			setReplacingTelegramToken(false)
			setClearTelegramToken(false)
			setStale(false)
			configurationFingerprint.current = ''
			dirtyRef.current = false
			void loadConfiguration(true)
		}, 0)
		return () => {
			window.clearTimeout(initial)
			loadController.current?.abort()
		}
	}, [instance.id, loadConfiguration])

	useEffect(() => {
		if (configuration?.status !== 'PENDING') return
		let stopped = false
		let timer = 0
		const poll = async () => {
			const value = await loadConfiguration(false)
			if (value && value.status !== 'PENDING') {
				hydrateForm(value)
				await onChanged()
				return
			}
			if (!stopped) timer = window.setTimeout(() => void poll(), 3000)
		}
		timer = window.setTimeout(() => void poll(), 3000)
		return () => {
			stopped = true
			window.clearTimeout(timer)
			loadController.current?.abort()
		}
	}, [configuration?.status, hydrateForm, loadConfiguration, onChanged])

	useEffect(() => {
		if (lastRefreshSignal.current === refreshSignal) return
		lastRefreshSignal.current = refreshSignal
		void loadConfiguration(false)
	}, [loadConfiguration, refreshSignal])

	const updateForm = <Key extends keyof MessagingFormState>(key: Key, value: MessagingFormState[Key]) => {
		setForm((current) => ({ ...current, [key]: value }))
	}

	const save = async (event: FormEvent) => {
		event.preventDefault()
		if (!configuration || !dirty || validationError || stale || blocked) return
		setSaving(true)
		setError('')
		try {
			const submittedForm = form
			const replacementToken = telegramBotToken.trim()
			const operation = await apiRequest<Operation>(token, `/api/v1/instances/${instance.id}/messaging`, {
				method: 'PUT',
				body: JSON.stringify({
					telegram: {
						enabled: form.telegramEnabled,
						...(form.telegramEnabled && replacementToken ? { bot_token: replacementToken } : {}),
						clear_bot_token: clearTelegramToken,
						allowed_users: parseManagedList(form.telegramAllowedUsers),
						group_allowed_users: parseManagedList(form.telegramGroupAllowedUsers),
						group_allowed_chats: parseManagedList(form.telegramGroupAllowedChats),
						require_mention: form.telegramRequireMention,
						proxy_url: form.telegramProxyURL.trim(),
					},
					whatsapp: {
						enabled: form.whatsAppEnabled,
						mode: form.whatsAppMode,
						allowed_users: parseManagedList(form.whatsAppAllowedUsers),
						unauthorized_dm_behavior: form.whatsAppUnauthorizedDMBehavior,
						reply_prefix: form.whatsAppReplyPrefix,
					},
				}),
			})
			onOperation(operation)
			setConfiguration((current) => current ? { ...current, status: 'PENDING', last_error: undefined } : current)
			setSavedForm(submittedForm)
			setTelegramBotToken('')
			setReplacingTelegramToken(false)
			setClearTelegramToken(false)
			setStale(false)
			dirtyRef.current = false
			await onChanged()
			await loadConfiguration(true)
		} catch (requestError) {
			setError(requestError instanceof Error ? requestError.message : 'Messaging configuration could not be applied')
		} finally {
			setSaving(false)
		}
	}

	const pending = configuration?.status === 'PENDING'
	const controlsDisabled = saving || pending || blocked || !configuration
	const statusValue = configuration?.status ?? 'UNKNOWN'
	const statusLabel = loading
		? 'Loading'
		: ({ NOT_CONFIGURED: 'Not configured', PENDING: 'Applying to runtime', APPLIED: 'Applied to runtime', FAILED: 'Apply failed' } as Record<string, string>)[statusValue] ?? sentenceCase(statusValue)

	const toggleTelegram = (enabled: boolean) => {
		updateForm('telegramEnabled', enabled)
		if (!enabled) {
			setTelegramBotToken('')
			setReplacingTelegramToken(false)
		}
		if (enabled) setClearTelegramToken(false)
	}

	const reloadSavedConfiguration = () => {
		if (configuration) hydrateForm(configuration)
		else void loadConfiguration(true)
	}

	return <div className="profile-tab-content">
		<section className="section-block first-section profile-section messaging-section">
			<div className="section-heading">
					<div><h2>Messaging</h2><p>Hermes messaging settings for {instance.name}</p></div>
				<Status value={statusValue} label={statusLabel} />
			</div>
			{loading ? <div className="compact-empty"><LoaderCircle className="spin" size={18} /><div><strong>Loading messaging settings</strong><span>Reading the encrypted Fleet configuration.</span></div></div> :
				<form className="messaging-form" onSubmit={(event) => void save(event)}>
					<div className="messaging-channel-grid">
						<section className="messaging-channel-card">
							<div className="messaging-channel-heading">
								<div><strong>Telegram bot</strong><span>Bot token and explicit user or group access</span></div>
								<label className="switch-label"><input type="checkbox" checked={form.telegramEnabled} onChange={(event) => toggleTelegram(event.target.checked)} disabled={controlsDisabled} /><span>Enabled</span></label>
							</div>
							<div className="messaging-fields">
								{configuration?.telegram.token_configured && !replacingTelegramToken ? <div className={`messaging-secret-summary${clearTelegramToken ? ' pending-delete' : ''}`}>
									<div><span>Bot token</span><code>{configuration.telegram.token_hint || '••••••••'}</code><small>{clearTelegramToken ? 'Will be deleted when changes are applied' : 'Stored encrypted by Fleet'}</small></div>
									{!clearTelegramToken && <button type="button" className="secondary-button" onClick={() => setReplacingTelegramToken(true)} disabled={controlsDisabled || !form.telegramEnabled}>Replace token</button>}
								</div> : <div className="messaging-token-editor">
									<label>{configuration?.telegram.token_configured ? 'New bot token' : 'Bot token'}
										<input type="password" autoComplete="new-password" value={telegramBotToken} onChange={(event) => { setTelegramBotToken(event.target.value); setClearTelegramToken(false) }} placeholder="123456789:bot-token" disabled={controlsDisabled || !form.telegramEnabled} />
										<small>The token is encrypted by Fleet and never returned to this page.</small>
									</label>
									{configuration?.telegram.token_configured && <button type="button" className="text-button" onClick={() => { setTelegramBotToken(''); setReplacingTelegramToken(false) }} disabled={controlsDisabled}>Cancel replacement</button>}
								</div>}
								{configuration?.telegram.token_configured && <label className="inline-check"><input type="checkbox" checked={clearTelegramToken} onChange={(event) => {
									const checked = event.target.checked
									setClearTelegramToken(checked)
									if (checked) {
										setTelegramBotToken('')
										setReplacingTelegramToken(false)
										updateForm('telegramEnabled', false)
									}
								}} disabled={controlsDisabled} /><span>Disable Telegram and delete the stored token</span></label>}
								<label>Allowed users
									<textarea value={form.telegramAllowedUsers} onChange={(event) => updateForm('telegramAllowedUsers', event.target.value)} placeholder={'Telegram user ID\nOne numeric ID per line'} disabled={controlsDisabled || !form.telegramEnabled} required={form.telegramEnabled} />
								</label>
								<details className="messaging-advanced">
									<summary>Group access and proxy</summary>
									<div className="messaging-fields">
										<label>Allowed users in groups<textarea value={form.telegramGroupAllowedUsers} onChange={(event) => updateForm('telegramGroupAllowedUsers', event.target.value)} placeholder="Numeric user IDs, one per line" disabled={controlsDisabled || !form.telegramEnabled} /></label>
										<label>Allowed group chats<textarea value={form.telegramGroupAllowedChats} onChange={(event) => updateForm('telegramGroupAllowedChats', event.target.value)} placeholder="-1001234567890" disabled={controlsDisabled || !form.telegramEnabled} /></label>
										<label>Proxy URL<input value={form.telegramProxyURL} onChange={(event) => updateForm('telegramProxyURL', event.target.value)} placeholder="socks5://127.0.0.1:1080" disabled={controlsDisabled || !form.telegramEnabled} /><small>Proxy credentials are intentionally rejected.</small></label>
										<label className="inline-check"><input type="checkbox" checked={form.telegramRequireMention} onChange={(event) => updateForm('telegramRequireMention', event.target.checked)} disabled={controlsDisabled || !form.telegramEnabled} /><span>Require @mention in group chats</span></label>
									</div>
								</details>
							</div>
						</section>

						<section className="messaging-channel-card">
							<div className="messaging-channel-heading">
								<div><strong>WhatsApp</strong><span>Hermes Baileys session and message access policy</span></div>
								<label className="switch-label"><input type="checkbox" checked={form.whatsAppEnabled} onChange={(event) => updateForm('whatsAppEnabled', event.target.checked)} disabled={controlsDisabled} /><span>Enabled</span></label>
							</div>
							<div className="messaging-fields">
								<label>Mode<select value={form.whatsAppMode} onChange={(event) => updateForm('whatsAppMode', event.target.value as MessagingFormState['whatsAppMode'])} disabled={controlsDisabled || !form.whatsAppEnabled}><option value="bot">Bot</option><option value="self-chat">Self-chat</option></select></label>
								<label>Allowed numbers
									<textarea value={form.whatsAppAllowedUsers} onChange={(event) => updateForm('whatsAppAllowedUsers', event.target.value)} placeholder={'628123456789\nCountry code without +'} disabled={controlsDisabled || !form.whatsAppEnabled || form.whatsAppMode === 'self-chat'} required={form.whatsAppEnabled && form.whatsAppMode === 'bot'} />
								</label>
								<label>Unknown direct messages<select value={form.whatsAppUnauthorizedDMBehavior} onChange={(event) => updateForm('whatsAppUnauthorizedDMBehavior', event.target.value as MessagingFormState['whatsAppUnauthorizedDMBehavior'])} disabled={controlsDisabled || !form.whatsAppEnabled}><option value="ignore">Ignore</option><option value="pair">Offer pairing</option></select></label>
								<label>Reply prefix<textarea className="compact-textarea" value={form.whatsAppReplyPrefix} onChange={(event) => updateForm('whatsAppReplyPrefix', event.target.value)} maxLength={240} disabled={controlsDisabled || !form.whatsAppEnabled} /></label>
							</div>
							<div className="messaging-notice"><ShieldCheck size={17} /><div><strong>Pairing is separate</strong><span>Fleet applies settings to the WhatsApp session stored inside this instance. It does not claim the session is paired or run the interactive QR flow. Hermes uses unofficial Baileys access; use a dedicated number.</span></div></div>
						</section>
					</div>
					{stale && <div className="messaging-notice"><RefreshCw size={17} /><div><strong>Saved settings changed while you were editing</strong><span>Reload the latest saved values before applying this draft.</span></div><button type="button" className="secondary-button compact-button" onClick={reloadSavedConfiguration} disabled={saving || pending}>Reload saved settings</button></div>}
					{validationError && dirty && <div className="inline-error" role="alert">{validationError}</div>}
					{configuration?.last_error && <div className="inline-error" role="alert">Host Agent: {configuration.last_error}</div>}
					{error && <div className="inline-error" role="alert">{error}</div>}
					<div className="messaging-footer">
						<div><strong>{pending ? 'Applying on the Host Agent' : blocked ? 'Wait for the current instance operation to finish.' : !dirty ? 'Saved settings are unchanged.' : instance.status === 'RUNNING' ? 'A save recreates and health-checks the running Hermes services.' : 'A save updates the stopped instance without starting it.'}</strong><span>Secrets stay out of operation metadata and Host Agent job payloads.</span></div>
						<button className="primary-button" type="submit" disabled={controlsDisabled || stale || !dirty || Boolean(validationError) || !['RUNNING', 'STOPPED'].includes(instance.status)}><MessageCircle size={16} />{pending ? 'Applying' : saving ? 'Queuing' : 'Save and apply'}</button>
					</div>
				</form>}
		</section>
	</div>
}

type MCPServerDraft = {
	originalName: string
	originalURL: string
	originalAuthType: 'none' | 'bearer'
	name: string
	url: string
	authType: 'none' | 'bearer'
	tokenConfigured: boolean
	tokenHint: string
	newToken: string
	clearToken: boolean
	enabled: boolean
	selectedTools: string[]
	availableTools: MCPDiscoveredTool[]
	inventoryComplete: boolean
	discovering: boolean
	discoveryIssue: {
		message: string
		stage: string
		action: string
	} | null
}

function normalizeMCPConfiguration(value: unknown): MCPConfiguration {
	const root = value && typeof value === 'object' ? value as Record<string, unknown> : {}
	const statuses: MCPConfiguration['status'][] = ['NOT_CONFIGURED', 'PENDING', 'APPLIED', 'FAILED']
	const status = statuses.includes(root.status as MCPConfiguration['status']) ? root.status as MCPConfiguration['status'] : 'NOT_CONFIGURED'
	const servers = Array.isArray(root.servers) ? root.servers.flatMap((candidate): MCPServerConfiguration[] => {
		if (!candidate || typeof candidate !== 'object') return []
		const server = candidate as Record<string, unknown>
		if (typeof server.name !== 'string' || typeof server.url !== 'string') return []
		return [{
			name: server.name,
			source: 'remote',
			url: server.url,
			auth_type: server.auth_type === 'bearer' ? 'bearer' : 'none',
			token_configured: server.token_configured === true,
			token_hint: typeof server.token_hint === 'string' ? server.token_hint : '',
			enabled: server.enabled === true,
			tools: Array.isArray(server.tools) ? server.tools.filter((tool): tool is string => typeof tool === 'string') : [],
		}]
	}) : []
	const optionalString = (candidate: unknown) => typeof candidate === 'string' && candidate ? candidate : undefined
	return {
		status,
		servers,
		...(optionalString(root.last_error) ? { last_error: optionalString(root.last_error) } : {}),
		...(optionalString(root.desired_revision) ? { desired_revision: optionalString(root.desired_revision) } : {}),
		...(optionalString(root.applied_revision) ? { applied_revision: optionalString(root.applied_revision) } : {}),
		...(optionalString(root.updated_at) ? { updated_at: optionalString(root.updated_at) } : {}),
		...(optionalString(root.applied_at) ? { applied_at: optionalString(root.applied_at) } : {}),
	}
}

function mcpDrafts(configuration: MCPConfiguration): MCPServerDraft[] {
	return configuration.servers.map((server) => ({
		originalName: server.name,
		originalURL: server.url,
		originalAuthType: server.auth_type,
		name: server.name,
		url: server.url,
		authType: server.auth_type,
		tokenConfigured: server.token_configured,
		tokenHint: server.token_hint,
		newToken: '',
		clearToken: false,
		enabled: server.enabled,
		selectedTools: [...server.tools],
		availableTools: server.tools.map((name) => ({ name })),
		inventoryComplete: true,
		discovering: false,
		discoveryIssue: null,
	}))
}

function mcpDraftFingerprint(drafts: MCPServerDraft[]) {
	return JSON.stringify(drafts.map((draft) => ({
		originalName: draft.originalName,
		name: draft.name,
		url: draft.url,
		authType: draft.authType,
		tokenConfigured: draft.tokenConfigured,
		tokenHint: draft.tokenHint,
		clearToken: draft.clearToken,
		enabled: draft.enabled,
		selectedTools: [...draft.selectedTools].sort(),
	})))
}

function mcpStoredTokenReusable(draft: MCPServerDraft) {
	return draft.tokenConfigured && !draft.clearToken && draft.originalAuthType === 'bearer' &&
		draft.name.trim().toLowerCase() === draft.originalName && draft.url.trim() === draft.originalURL && draft.authType === draft.originalAuthType
}

function validateMCPDrafts(drafts: MCPServerDraft[]) {
	if (drafts.length > 20) return 'An instance can have at most 20 MCP servers.'
	const names = new Set<string>()
	for (const draft of drafts) {
		const name = draft.name.trim().toLowerCase()
		if (!/^[a-z][a-z0-9_-]{1,31}$/.test(name)) return 'Server names must use 2-32 lowercase letters, numbers, underscores, or hyphens.'
		if (names.has(name)) return 'MCP server names must be unique.'
		names.add(name)
		try {
			const endpoint = new URL(draft.url.trim())
			if (endpoint.protocol !== 'https:' || !endpoint.host || endpoint.username || endpoint.password || endpoint.hash) return 'Each MCP endpoint must be an HTTPS URL without credentials or a fragment.'
		} catch {
			return 'Each MCP endpoint must be a valid HTTPS URL.'
		}
		const tools = draft.selectedTools
		if (draft.authType === 'bearer' && draft.enabled) {
			const retainingToken = mcpStoredTokenReusable(draft)
			if (!draft.newToken.trim() && !retainingToken) return `Enter a bearer token for ${name}.`
		}
		if (draft.enabled && !draft.inventoryComplete) return `Connect to ${name || 'this server'} and discover its tools before installing.`
		if (draft.enabled && tools.length === 0) return `Add an explicit tool allowlist before enabling ${name}.`
		if (tools.length > 100 || tools.some((tool) => !/^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$/.test(tool))) return `The tool allowlist for ${name} is invalid.`
	}
	return ''
}

function validateMCPConnectionDraft(draft: MCPServerDraft) {
	const name = draft.name.trim().toLowerCase()
	if (!/^[a-z][a-z0-9_-]{1,31}$/.test(name)) return 'Enter a valid server name first.'
	try {
		const endpoint = new URL(draft.url.trim())
		if (endpoint.protocol !== 'https:' || !endpoint.host || endpoint.username || endpoint.password || endpoint.hash) return 'Enter a safe HTTPS MCP endpoint first.'
	} catch {
		return 'Enter a valid HTTPS MCP endpoint first.'
	}
	const retainedTokenMatchesEndpoint = mcpStoredTokenReusable(draft)
	if (draft.authType === 'bearer' && !draft.newToken.trim() && !retainedTokenMatchesEndpoint) return 'Enter a bearer token before discovering tools.'
	return ''
}

function MCPSettings({
	instance,
	token,
	onChanged,
	onOperation,
	refreshSignal,
	blocked = false,
}: {
	instance: Instance
	token: string
	onChanged: () => Promise<void>
	onOperation: (operation: Operation) => void
	refreshSignal: number
	blocked?: boolean
}) {
	const [configuration, setConfiguration] = useState<MCPConfiguration | null>(null)
	const [drafts, setDrafts] = useState<MCPServerDraft[]>([])
	const [editingIndex, setEditingIndex] = useState<number | null>(null)
	const [savedFingerprint, setSavedFingerprint] = useState('[]')
	const [loading, setLoading] = useState(true)
	const [saving, setSaving] = useState(false)
	const [error, setError] = useState('')
	const [stale, setStale] = useState(false)
	const lastRefreshSignal = useRef(refreshSignal)
	const dirtyRef = useRef(false)
	const revisionRef = useRef('')
	const dirty = mcpDraftFingerprint(drafts) !== savedFingerprint || drafts.some((draft) => draft.newToken.trim() !== '')
	const validationError = validateMCPDrafts(drafts)
	const pending = configuration?.status === 'PENDING'
	const retryable = configuration?.status === 'FAILED' && !dirty
	const controlsDisabled = loading || saving || pending || blocked

	const hydrate = useCallback((value: MCPConfiguration) => {
		const nextDrafts = mcpDrafts(value)
		setConfiguration(value)
		setDrafts(nextDrafts)
		setEditingIndex(value.status === 'FAILED' && nextDrafts.length > 0 ? 0 : null)
		setSavedFingerprint(mcpDraftFingerprint(nextDrafts))
		setStale(false)
		dirtyRef.current = false
		revisionRef.current = value.desired_revision ?? ''
	}, [])

	useEffect(() => {
		dirtyRef.current = dirty
	}, [dirty])

	const load = useCallback(async (replace: boolean) => {
		try {
			const value = normalizeMCPConfiguration(await apiRequest<unknown>(token, `/api/v1/instances/${instance.id}/mcp`, { cache: 'no-store' }))
			if (replace || !dirtyRef.current) hydrate(value)
			else if ((value.desired_revision ?? '') !== revisionRef.current) {
				setConfiguration(value)
				setStale(true)
			}
			setError('')
			return value
		} catch (requestError) {
			setError(requestError instanceof Error ? requestError.message : 'MCP configuration could not be loaded')
			return null
		} finally {
			setLoading(false)
		}
	}, [hydrate, instance.id, token])

	useEffect(() => {
		const initial = window.setTimeout(() => {
			setLoading(true)
			setConfiguration(null)
			setDrafts([])
			setEditingIndex(null)
			setSavedFingerprint('[]')
			setStale(false)
			dirtyRef.current = false
			revisionRef.current = ''
			void load(true)
		}, 0)
		return () => window.clearTimeout(initial)
	}, [instance.id, load])

	useEffect(() => {
		if (!pending) return
		let stopped = false
		let timer = 0
		const poll = async () => {
			const value = await load(true)
			if (value && value.status !== 'PENDING') {
				await onChanged()
				return
			}
			if (!stopped) timer = window.setTimeout(() => void poll(), 3000)
		}
		timer = window.setTimeout(() => void poll(), 3000)
		return () => { stopped = true; window.clearTimeout(timer) }
	}, [load, onChanged, pending])

	useEffect(() => {
		if (lastRefreshSignal.current === refreshSignal) return
		lastRefreshSignal.current = refreshSignal
		void load(false)
	}, [load, refreshSignal])

	const updateDraft = <Key extends keyof MCPServerDraft>(index: number, key: Key, value: MCPServerDraft[Key]) => {
		setDrafts((current) => current.map((draft, draftIndex) => draftIndex === index ? { ...draft, [key]: value } : draft))
	}
	const updateConnectionDraft = <Key extends 'url' | 'authType' | 'newToken' | 'clearToken'>(index: number, key: Key, value: MCPServerDraft[Key]) => {
		setDrafts((current) => current.map((draft, draftIndex) => {
			if (draftIndex !== index) return draft
			const next = { ...draft, [key]: value }
			const installedConnection = Boolean(next.originalName) && next.url === next.originalURL && next.authType === next.originalAuthType && !next.newToken.trim() && !next.clearToken
			return { ...next, inventoryComplete: installedConnection, discoveryIssue: null }
		}))
	}

	const addServer = () => {
		const nextIndex = drafts.length
		setDrafts((current) => [...current, {
			originalName: '', originalURL: '', originalAuthType: 'none', name: '', url: '', authType: 'none', tokenConfigured: false, tokenHint: '',
			newToken: '', clearToken: false, enabled: true, selectedTools: [], availableTools: [],
			inventoryComplete: false, discovering: false, discoveryIssue: null,
		}])
		setEditingIndex(nextIndex)
	}

	const removeServer = (index: number) => {
		setDrafts((current) => current.filter((_, draftIndex) => draftIndex !== index))
		setEditingIndex((current) => current === index ? null : current !== null && current > index ? current - 1 : current)
	}

	const cancelEditing = (index: number) => {
		setDrafts((current) => {
			const draft = current[index]
			if (!draft) return current
			if (!draft.originalName) return current.filter((_, draftIndex) => draftIndex !== index)
			const saved = configuration ? mcpDrafts(configuration).find((candidate) => candidate.originalName === draft.originalName) : undefined
			if (!saved) return current
			return current.map((candidate, draftIndex) => draftIndex === index ? saved : candidate)
		})
		setEditingIndex(null)
	}

	const discoverTools = async (index: number) => {
		const draft = drafts[index]
		if (!draft) return
		const connectionError = validateMCPConnectionDraft(draft)
		if (connectionError) {
			updateDraft(index, 'discoveryIssue', { message: connectionError, stage: 'Validate connection', action: 'Correct the connection details, then retry.' })
			return
		}
		setDrafts((current) => current.map((candidate, draftIndex) => draftIndex === index ? { ...candidate, discovering: true, discoveryIssue: null } : candidate))
		try {
			const result = await apiRequest<MCPDiscoveryResult>(token, `/api/v1/instances/${instance.id}/mcp/discover`, {
				method: 'POST',
				body: JSON.stringify({
					original_name: draft.originalName, name: draft.name.trim().toLowerCase(), url: draft.url.trim(), auth_type: draft.authType,
					...(draft.newToken.trim() ? { bearer_token: draft.newToken.trim() } : {}),
				}),
			})
			const seen = new Set<string>()
			const availableTools = (Array.isArray(result.tools) ? result.tools : []).filter((tool) => {
				if (!tool || typeof tool.name !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$/.test(tool.name) || seen.has(tool.name)) return false
				seen.add(tool.name)
				return true
			}).map((tool) => ({ name: tool.name, ...(typeof tool.description === 'string' ? { description: tool.description } : {}) }))
			setDrafts((current) => current.map((candidate, draftIndex) => {
				if (draftIndex !== index) return candidate
				const selectedTools = candidate.selectedTools.length === 0
					? availableTools.map((tool) => tool.name)
					: candidate.selectedTools.filter((name) => seen.has(name))
				return { ...candidate, availableTools, selectedTools, inventoryComplete: true, discovering: false, discoveryIssue: null }
			}))
		} catch (requestError) {
			const message = requestError instanceof Error ? requestError.message : 'MCP tools could not be discovered.'
			const stage = requestError instanceof ApiError && requestError.stage ? requestError.stage : 'Connect to MCP server'
			const action = requestError instanceof ApiError && requestError.action ? requestError.action : 'Check the remote MCP server and retry.'
			setDrafts((current) => current.map((candidate, draftIndex) => draftIndex === index ? {
				...candidate, discovering: false, discoveryIssue: { message, stage, action },
			} : candidate))
		}
	}

	const toggleTool = (index: number, toolName: string, selected: boolean) => {
		setDrafts((current) => current.map((draft, draftIndex) => {
			if (draftIndex !== index) return draft
			const selectedTools = selected
				? [...new Set([...draft.selectedTools, toolName])]
				: draft.selectedTools.filter((name) => name !== toolName)
			return { ...draft, selectedTools }
		}))
	}

	const save = async (event: FormEvent) => {
		event.preventDefault()
		if ((!dirty && !retryable) || validationError || stale || controlsDisabled || !['RUNNING', 'STOPPED'].includes(instance.status)) return
		setSaving(true)
		setError('')
		try {
			const operation = await apiRequest<Operation>(token, `/api/v1/instances/${instance.id}/mcp`, {
				method: 'PUT',
				body: JSON.stringify({ servers: drafts.map((draft) => ({
					name: draft.name.trim().toLowerCase(), source: 'remote', url: draft.url.trim(), auth_type: draft.authType,
					...(draft.newToken.trim() ? { bearer_token: draft.newToken.trim() } : {}),
					clear_bearer_token: draft.clearToken, enabled: draft.enabled, tools: draft.selectedTools,
				})) }),
			})
			onOperation(operation)
			setConfiguration((current) => current ? { ...current, status: 'PENDING', last_error: undefined } : current)
			await onChanged()
			await load(true)
		} catch (requestError) {
			setError(requestError instanceof Error ? requestError.message : 'MCP configuration could not be applied')
		} finally {
			setSaving(false)
		}
	}

	const statusValue = configuration?.status ?? 'UNKNOWN'
	const statusLabel = loading ? 'Loading' : statusValue === 'APPLIED' && drafts.length === 0 ? 'No servers' : ({
		NOT_CONFIGURED: 'Not configured', PENDING: 'Installing and verifying', APPLIED: configuration?.applied_at ? `Installed · verified ${relativeTime(configuration.applied_at)}` : 'Installed', FAILED: 'Install failed',
	} as Record<string, string>)[statusValue] ?? sentenceCase(statusValue)
	const enabledCount = drafts.filter((draft) => draft.enabled).length

	return <div className="profile-tab-content">
		<section className="section-block first-section profile-section mcp-section">
			<div className="section-heading">
				<div><h2>MCP</h2><p>{drafts.length === 0 ? `Remote tool servers for ${instance.name}` : `${drafts.length} servers · ${enabledCount} enabled`}</p></div>
				<div className="section-actions"><Status value={statusValue} label={statusLabel} /><button type="button" className="primary-button compact-button" onClick={addServer} disabled={controlsDisabled || drafts.length >= 20}><Plus size={16} />Add server</button></div>
			</div>
			<div className="backup-scope"><ShieldCheck size={18} /><div><strong>Explicit tools only</strong><span>Fleet accepts HTTPS MCP endpoints and exposes only the tools listed here. Local commands, arbitrary package installation, sampling, and server-initiated elicitation are blocked.</span></div></div>
			{loading ? <div className="compact-empty"><LoaderCircle className="spin" size={18} /><div><strong>Loading MCP servers</strong><span>Reading the encrypted Fleet configuration.</span></div></div> : <form className="mcp-form" onSubmit={(event) => void save(event)}>
				{drafts.length === 0 ? <div className="compact-empty"><Plug size={18} /><div><strong>No MCP servers installed</strong><span>Add a remote HTTPS MCP server to give Hermes an explicit set of external tools.</span></div></div> : <div className="mcp-server-list">{drafts.map((draft, index) => {
					const editing = editingIndex === index
					const connectionError = validateMCPConnectionDraft(draft)
					const savedDraft = configuration ? mcpDrafts(configuration).find((candidate) => candidate.originalName === draft.originalName) : undefined
					const editorDirty = !savedDraft || mcpDraftFingerprint([draft]) !== mcpDraftFingerprint([savedDraft]) || Boolean(draft.newToken.trim())
					const tokenReplacementRequired = draft.authType === 'bearer' && !draft.newToken.trim() && !mcpStoredTokenReusable(draft)
					const tokenRequirementMessage = draft.originalName
						? 'The server name or endpoint changed. Enter a new token to connect.'
						: 'Enter a bearer token to connect.'
					const tokenHelpID = `mcp-token-help-${index}`
					return <section className={`mcp-server-card${editing ? ' editing' : ''}`} key={`${draft.originalName || 'new'}-${index}`}>
						<div className="mcp-server-summary">
							<div className="mcp-server-identity"><strong>{draft.name.trim() || 'New MCP server'}</strong><span>{draft.url.trim() || 'Connection details required'}</span></div>
							<div className="mcp-server-facts"><span>{draft.authType === 'bearer' ? 'Bearer auth' : 'No auth'}</span><span>{draft.selectedTools.length} {draft.selectedTools.length === 1 ? 'tool' : 'tools'}</span></div>
							<div className="mcp-server-actions"><label className="switch-label"><input type="checkbox" checked={draft.enabled} onChange={(event) => updateDraft(index, 'enabled', event.target.checked)} disabled={controlsDisabled} /><span>{draft.enabled ? 'Enabled' : 'Disabled'}</span></label><button type="button" className="secondary-button compact-button" onClick={() => editing ? cancelEditing(index) : setEditingIndex(index)} disabled={controlsDisabled}>{editing ? editorDirty ? 'Cancel changes' : 'Close' : 'Configure'}</button></div>
						</div>
						{editing && <div className="mcp-server-editor">
							<div className="form-grid mcp-server-fields">
								<label>Name<input value={draft.name} onChange={(event) => updateDraft(index, 'name', event.target.value.toLowerCase())} placeholder="linear" disabled={controlsDisabled || draft.discovering} required /></label>
								<label>Remote MCP URL<input type="url" value={draft.url} onChange={(event) => updateConnectionDraft(index, 'url', event.target.value)} placeholder="https://mcp.example.com/mcp" disabled={controlsDisabled || draft.discovering} required /></label>
								<label>Authentication<select value={draft.authType} onChange={(event) => updateConnectionDraft(index, 'authType', event.target.value as MCPServerDraft['authType'])} disabled={controlsDisabled || draft.discovering}><option value="none">None</option><option value="bearer">Bearer token</option></select></label>
								{draft.authType === 'bearer' && <div className="mcp-token-editor"><label>{draft.tokenConfigured && !draft.clearToken ? 'Replace bearer token' : 'Bearer token'}<input type="password" autoComplete="new-password" value={draft.newToken} onChange={(event) => { updateConnectionDraft(index, 'newToken', event.target.value); updateConnectionDraft(index, 'clearToken', false) }} placeholder={tokenReplacementRequired ? 'Required to connect' : draft.tokenConfigured && !draft.clearToken ? draft.tokenHint || 'Stored encrypted by Fleet' : 'Required'} disabled={controlsDisabled || draft.discovering} aria-invalid={tokenReplacementRequired || undefined} aria-describedby={tokenHelpID} /><small id={tokenHelpID} className={tokenReplacementRequired ? 'field-error' : undefined}>{tokenReplacementRequired ? tokenRequirementMessage : draft.clearToken ? 'The stored token will be removed when this configuration is applied.' : draft.tokenConfigured && !draft.newToken ? `${draft.tokenHint || 'Token'} is stored encrypted and is not returned.` : 'The token is encrypted before it is stored by Fleet.'}</small></label>{draft.tokenConfigured && !draft.newToken && <button type="button" className="text-button" onClick={() => updateConnectionDraft(index, 'clearToken', !draft.clearToken)} disabled={controlsDisabled || draft.discovering}>{draft.clearToken ? 'Keep stored token' : 'Remove stored token'}</button>}</div>}
							</div>
							<div className="mcp-discovery-bar"><div><strong>Available tools</strong><span>{draft.inventoryComplete ? 'Discovered from this endpoint. Select only what Hermes may use.' : draft.originalName ? 'Refresh after changing the connection. The installed allowlist remains visible until then.' : 'Connect to load the tools exposed by this server.'}</span></div><button type="button" className="secondary-button compact-button" onClick={() => void discoverTools(index)} disabled={controlsDisabled || draft.discovering || Boolean(connectionError)}>{draft.discovering ? <LoaderCircle className="spin" size={15} /> : <RefreshCw size={15} />}{draft.discovering ? 'Connecting' : draft.discoveryIssue ? 'Retry connection' : draft.inventoryComplete ? 'Refresh tools' : 'Connect and discover'}</button></div>
							{draft.discoveryIssue && <div className="mcp-discovery-error" role="alert"><div><strong>{draft.discoveryIssue.stage}</strong><span>{draft.discoveryIssue.message}</span><small>{draft.discoveryIssue.action}</small></div></div>}
							{draft.availableTools.length > 0 ? <div className="mcp-tool-selector">{draft.availableTools.map((tool) => <label className="mcp-tool-option" key={tool.name}><input type="checkbox" checked={draft.selectedTools.includes(tool.name)} onChange={(event) => toggleTool(index, tool.name, event.target.checked)} disabled={controlsDisabled || draft.discovering || !draft.inventoryComplete} /><span><strong>{tool.name}</strong>{tool.description && <small>{tool.description}</small>}</span></label>)}</div> : <div className="compact-empty mcp-tool-empty"><Plug size={16} /><div><strong>No tool inventory loaded</strong><span>Connect and discover before enabling this server.</span></div></div>}
							<div className="mcp-editor-footer"><span>Tool names are read-only and come directly from MCP discovery.</span><button type="button" className="danger-button compact-button" onClick={() => removeServer(index)} disabled={controlsDisabled}><Trash2 size={14} />Remove server</button></div>
						</div>}
					</section>
				})}</div>}
				{stale && <div className="repair-callout"><RefreshCw size={18} /><div><strong>Saved MCP settings changed while you were editing</strong><span>Reload the latest configuration before applying this draft.</span></div><button type="button" className="secondary-button compact-button" onClick={() => void load(true)} disabled={controlsDisabled}>Reload</button></div>}
				{validationError && dirty && <div className="inline-error" role="alert">{validationError}</div>}
				{configuration?.last_error && <div className="inline-error" role="alert">Host Agent: {configuration.last_error}</div>}
				{error && <div className="inline-error" role="alert">{error}</div>}
				<div className="messaging-footer"><div><strong>{pending ? 'Installing and verifying on the Host Agent' : retryable ? 'The last installation failed. Retry is available without changing the configuration.' : !dirty ? drafts.length === 0 ? 'No MCP servers are configured.' : 'Installed MCP settings are unchanged.' : instance.status === 'RUNNING' ? 'Apply restarts and health-checks Hermes, then tests every enabled MCP server.' : 'Apply verifies the stopped instance without leaving it running.'}</strong><span>Bearer tokens stay out of the browser response, operation metadata, and job payload.</span></div><button className="primary-button" type="submit" disabled={controlsDisabled || stale || (!dirty && !retryable) || Boolean(validationError) || !['RUNNING', 'STOPPED'].includes(instance.status)}><Plug size={16} />{pending ? 'Installing' : saving ? 'Queuing' : retryable ? 'Retry installation' : drafts.length === 0 ? 'Remove all MCP servers' : 'Install and verify'}</button></div>
			</form>}
		</section>
	</div>
}

function parseManagedList(value: string) {
	return value.split(/[\s,]+/).map((item) => item.trim()).filter(Boolean)
}

function renderManagedList(value: string[] | null | undefined) {
	return Array.isArray(value) ? value.join('\n') : ''
}

function CodexAuthDialog({ instance, token, onClose, onConnected }: { instance: Instance; token: string; onClose: () => void; onConnected: () => void }) {
  const [session, setSession] = useState<CodexAuthSession | null>(null)
  const [error, setError] = useState('')
  const [copied, setCopied] = useState(false)
  const [canceling, setCanceling] = useState(false)
  const onConnectedRef = useRef(onConnected)
  const { dialogRef, onKeyDown } = useDialogAccessibility(onClose)

  useEffect(() => {
    onConnectedRef.current = onConnected
  }, [onConnected])

  useEffect(() => {
    const controller = new AbortController()
    const run = async () => {
      try {
        const started = await apiRequest<CodexAuthSession>(token, `/api/v1/instances/${instance.id}/codex-auth`, {
          method: 'POST',
          body: '{}',
          signal: controller.signal,
        })
        if (controller.signal.aborted) return
        setSession(started)
        for (let attempt = 0; attempt < 920 && !controller.signal.aborted; attempt += 1) {
          await sleep(1000, controller.signal)
          const current = await apiRequest<CodexAuthSession>(token, `/api/v1/instances/${instance.id}/codex-auth/${started.operation_id}`, {
            cache: 'no-store',
            signal: controller.signal,
          })
          if (controller.signal.aborted) return
          setSession(current)
          if (current.status === 'SUCCEEDED') {
            onConnectedRef.current()
            return
          }
          if (current.status === 'FAILED') {
            setError(current.error || 'Codex authentication failed')
            return
          }
        }
        if (!controller.signal.aborted) setError('Codex authentication timed out')
      } catch (requestError) {
        if (requestError instanceof DOMException && requestError.name === 'AbortError') return
        if (!controller.signal.aborted) setError(requestError instanceof Error ? requestError.message : 'Codex authentication could not be started')
      }
    }
    void run()
    return () => controller.abort()
  }, [instance.id, token])

  const copyCode = async () => {
    if (!session?.user_code) return
    await navigator.clipboard.writeText(session.user_code)
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1200)
  }

  const cancelAuthentication = async () => {
    if (!session || !['PENDING', 'RUNNING'].includes(session.status)) return
    setCanceling(true)
    setError('')
    try {
      await apiRequest<void>(token, `/api/v1/instances/${instance.id}/codex-auth/${session.operation_id}`, { method: 'DELETE' })
      setSession({ ...session, status: 'FAILED', stage: undefined, error: 'Codex authentication canceled by administrator' })
      setError('Codex authentication canceled')
    } catch (requestError) {
      setError(requestError instanceof Error ? requestError.message : 'Codex authentication could not be canceled')
    } finally {
      setCanceling(false)
    }
  }

  const complete = session?.status === 'SUCCEEDED'
  const active = ['PENDING', 'RUNNING'].includes(session?.status ?? '')
  const awaitingUser = active && session?.stage === 'AWAITING_USER' && Boolean(session.user_code && session.verification_uri)
  return <div className="modal-backdrop" role="presentation"><div ref={dialogRef} className="modal codex-auth-modal" role="dialog" aria-modal="true" aria-labelledby="codex-auth-title" tabIndex={-1} onKeyDown={onKeyDown}><div className="modal-header"><div><h2 id="codex-auth-title">Authenticate Codex</h2><p>{instance.name} · managed by Hermes Fleet</p></div><button className="icon-button" onClick={onClose} title="Close"><X size={18} /></button></div><div className="modal-body codex-auth-body">{complete ? <div className="auth-result auth-success"><ShieldCheck size={28} /><div><strong>Codex is connected</strong><span>New Hermes sessions can now use the configured Codex model.</span></div></div> : awaitingUser ? <><div className="auth-instruction"><span>1</span><div><strong>Open OpenAI sign-in</strong><p>Approve this instance in your browser. Fleet keeps the OAuth session isolated inside this Hermes instance.</p></div></div><a className="primary-button full-button" href={session?.verification_uri} target="_blank" rel="noreferrer"><ExternalLink size={17} />Open OpenAI sign-in</a><div className="auth-instruction"><span>2</span><div><strong>Enter this one-time code</strong><p>The code expires {session?.expires_at ? relativeTimeFuture(session.expires_at) : 'soon'}.</p></div></div><button className="device-code" onClick={() => void copyCode()}><code>{session?.user_code}</code><span><Copy size={16} />{copied ? 'Copied' : 'Copy code'}</span></button><div className="auth-waiting"><RefreshCw size={16} className="spin" />Waiting for OpenAI approval</div></> : <div className="auth-result"><RefreshCw size={24} className={error ? '' : 'spin'} /><div><strong>{error ? 'Authentication could not continue' : session?.stage === 'VERIFYING' ? 'Verifying Codex session' : 'Starting secure sign-in'}</strong><span>{error || 'Fleet is preparing a one-time OpenAI device code.'}</span></div></div>}{error && awaitingUser && <div className="inline-error">{error}</div>}<div className="modal-actions">{active && <button className="secondary-button danger-button" onClick={() => void cancelAuthentication()} disabled={canceling}>{canceling ? 'Canceling' : 'Cancel authentication'}</button>}<button className="secondary-button" onClick={onClose}>{complete ? 'Done' : 'Close'}</button></div></div></div></div>
}

function DetailRow({ label, value, mono = false, wide = false }: { label: string; value: string; mono?: boolean; wide?: boolean }) {
  return <div className={`detail-row ${wide ? 'detail-wide' : ''}`}><span>{label}</span>{mono ? <code title={value}>{value}</code> : <strong>{value}</strong>}</div>
}

function HermesUpdateProgress({ flow }: { flow: HermesUpdateFlow }) {
	const steps = hermesUpdateSteps(flow)
	return <>
		<p className="sr-only" role="status" aria-live="polite">Step {Math.min(flow.step + 1, steps.length)} of {steps.length}: {flow.detail}</p>
		<ol className="hermes-update-steps" aria-label={flow.kind === 'RUNTIME_REFRESH' ? 'Managed runtime maintenance progress' : 'Hermes update progress'}>
			{steps.map((label, index) => {
				const state = index < flow.step || flow.status === 'success' ? 'complete' : index === flow.step ? flow.status : 'pending'
				const stateLabel = state === 'complete' ? 'Completed' : state === 'running' ? 'In progress' : state === 'error' ? 'Stopped here' : 'Waiting'
				return <li key={label} className={state} aria-current={state === 'running' || state === 'error' ? 'step' : undefined}>
					<span className="hermes-step-marker" aria-hidden="true">{state === 'complete' ? <Check size={13} /> : state === 'running' ? <LoaderCircle className="spin" size={13} /> : state === 'error' ? <X size={13} /> : index + 1}</span>
					<span className="hermes-step-copy"><strong>{label}</strong><small>{stateLabel}</small></span>
				</li>
			})}
		</ol>
	</>
}

function CredentialRow({ label, value, initiallyVisible = false }: { label: string; value: string; initiallyVisible?: boolean }) {
  const [visible, setVisible] = useState(initiallyVisible)
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    await navigator.clipboard.writeText(value)
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1200)
  }
  return <div className="credential-row"><span>{label}</span><code>{visible ? value : '****************'}</code><div><button className="icon-button" onClick={() => setVisible(!visible)} title={visible ? `Hide ${label}` : `Show ${label}`}>{visible ? <EyeOff size={16} /> : <Eye size={16} />}</button><button className="icon-button" onClick={() => void copy()} title={`Copy ${label}`}><Copy size={16} /></button></div>{copied && <small>Copied</small>}</div>
}

function AlertsView({ records, loading, error, onNavigate }: { records: FleetAlertRecord[]; loading: boolean; error: string; onNavigate: (target: FleetAlertRecord['action']) => void }) {
	const [stateFilter, setStateFilter] = useState('ALL')
	const [severityFilter, setSeverityFilter] = useState('ALL')
	const [selectedID, setSelectedID] = useState('')
	const [showAllHistory, setShowAllHistory] = useState(false)
	const active = records.filter((record) => record.state === 'ACTIVE')
	const history = records.filter((record) => record.state !== 'ACTIVE')
	const filtered = records.filter((record) => (stateFilter === 'ALL' || (stateFilter === 'ACTIVE' ? record.state === 'ACTIVE' : record.state !== 'ACTIVE')) && (severityFilter === 'ALL' || record.severity === severityFilter))
	const filteredHistory = filtered.filter((record) => record.state !== 'ACTIVE')
	const visibleHistoryIDs = new Set((showAllHistory ? filteredHistory : filteredHistory.slice(0, 5)).map((record) => record.id))
	const visible = filtered.filter((record) => record.state === 'ACTIVE' || visibleHistoryIDs.has(record.id))
	const hiddenHistory = filteredHistory.length - visibleHistoryIDs.size
	const selected = records.find((record) => record.id === selectedID)
	return <section className="section-block first-section alerts-section">
		<div className="section-heading"><div><h2>Alerts &amp; incidents</h2><p>Actionable current state and recent recovery history</p></div>{loading && <RefreshCw className="spin" size={17} aria-label="Loading alert sources" />}</div>
		<div className="alert-summary-band" aria-label="Alert summary">
			<div><span>Active</span><strong>{active.length}</strong></div>
			<div><span>Critical</span><strong>{active.filter((record) => record.severity === 'CRITICAL').length}</strong></div>
			<div><span>Warning</span><strong>{active.filter((record) => record.severity === 'WARNING').length}</strong></div>
			<div><span>Recent incidents</span><strong>{history.length}</strong></div>
		</div>
		<div className="alerts-toolbar">
			<label><span>State</span><select aria-label="Alert state" value={stateFilter} onChange={(event) => setStateFilter(event.target.value)}><option value="ALL">All states</option><option value="ACTIVE">Active</option><option value="HISTORY">History</option></select></label>
			<label><span>Severity</span><select aria-label="Alert severity" value={severityFilter} onChange={(event) => setSeverityFilter(event.target.value)}><option value="ALL">All severity</option><option value="CRITICAL">Critical</option><option value="WARNING">Warning</option></select></label>
		</div>
		{error && <div className="alert-source-warning"><Activity size={16} /><div><strong>Some alert sources are unavailable</strong><span>{error}</span></div></div>}
		{visible.length === 0 ? <EmptyState icon={ShieldCheck} title={loading ? 'Checking Fleet health' : stateFilter === 'ACTIVE' ? 'No active alerts' : 'No matching alerts'} detail={loading ? 'Fleet is loading runtime, backup, and infrastructure state.' : 'Current authoritative checks do not match this filter.'} /> : <div className="alert-list">{visible.map((record) => <button key={record.id} className={selectedID === record.id ? 'selected' : undefined} onClick={() => setSelectedID(record.id)} aria-label={`Open alert: ${record.title}`}>
			<span className={`alert-severity-mark ${record.severity.toLowerCase()}`} aria-hidden="true" />
			<span className="alert-list-copy"><strong>{record.title}</strong><small>{record.detail}</small><em>{record.source} · {relativeTime(record.detectedAt)}</em></span>
			<span className="alert-list-status"><Status value={alertStatusValue(record)} label={alertStatusLabel(record)} /><ChevronRight size={16} /></span>
		</button>)}</div>}
		{filteredHistory.length > 5 && <div className="alert-history-footer"><button type="button" className="secondary-button compact-button" onClick={() => setShowAllHistory((current) => !current)}>{showAllHistory ? 'Show recent only' : `Show all history (${filteredHistory.length})`}</button>{hiddenHistory > 0 && <span>{hiddenHistory} older incidents hidden</span>}</div>}
		{selected && <AlertDetailPanel record={selected} onClose={() => setSelectedID('')} onNavigate={onNavigate} />}
	</section>
}

function alertStatusValue(record: FleetAlertRecord) {
	if (record.state === 'ACTIVE') return record.severity === 'CRITICAL' ? 'FAILED' : 'PENDING'
	return record.state === 'RECOVERED' ? 'READY' : 'FAILED'
}

function alertStatusLabel(record: FleetAlertRecord) {
	if (record.state === 'ACTIVE') return record.severity === 'CRITICAL' ? 'Critical' : 'Warning'
	if (record.resolution === 'SUPERSEDED') return 'Superseded'
	return record.state === 'RECOVERED' ? 'Recovered' : 'Failed'
}

function AlertDetailPanel({ record, onClose, onNavigate }: { record: FleetAlertRecord; onClose: () => void; onNavigate: (target: FleetAlertRecord['action']) => void }) {
	const { dialogRef, onKeyDown } = useDialogAccessibility(onClose)
	return createPortal(<div className="operation-detail-backdrop" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose() }}>
		<aside ref={dialogRef} className="operation-detail-panel alert-detail-panel" role="dialog" aria-modal="true" aria-labelledby="alert-detail-title" tabIndex={-1} onKeyDown={onKeyDown}>
			<div className="operation-detail-heading"><div><span>{record.state === 'ACTIVE' ? 'Active alert' : 'Incident history'}</span><strong id="alert-detail-title">{record.title}</strong></div><button className="icon-button" onClick={onClose} title="Close alert details" aria-label="Close alert details"><X size={18} /></button></div>
			<div className="operation-detail-status"><span>{record.source}</span><Status value={alertStatusValue(record)} label={alertStatusLabel(record)} /></div>
			<div className="alert-detail-copy"><strong>What happened</strong><p>{record.detail}</p></div>
			<div className="alert-detail-evidence"><strong>Evidence</strong><ul>{record.evidence.map((item) => <li key={item}>{item}</li>)}</ul></div>
			<div className="operation-detail-grid"><div><span>Detected</span><strong>{relativeTime(record.detectedAt)}</strong></div><div><span>State</span><strong>{record.state === 'ACTIVE' ? 'Active' : record.resolution === 'SUPERSEDED' ? 'Superseded by success' : record.state === 'RECOVERED' ? 'Recovered' : 'Recorded failure'}</strong></div></div>
			<div className="alert-detail-actions"><button className="primary-button" onClick={() => { onClose(); onNavigate(record.action) }}>{record.action.label}<ChevronRight size={16} /></button></div>
			<div className="alert-authority-note"><ShieldCheck size={16} /><span>Alert state is derived from authoritative Fleet checks. It cannot be resolved manually.</span></div>
		</aside>
	</div>, document.body)
}

function HostsView({ hosts, instances, operations, token, refreshSignal, onSelectInstance }: {
	hosts: Host[]
	instances: Instance[]
	operations: Operation[]
	token: string
	refreshSignal: number
	onSelectInstance: (instanceID: string) => void
}) {
	const [runtimeHealth, setRuntimeHealth] = useState<RuntimeHealth | null>(null)
	const [telemetryError, setTelemetryError] = useState('')
	const [selectedHostID, setSelectedHostID] = useState('')
	useEffect(() => {
		const controller = new AbortController()
		void apiRequest<RuntimeHealth>(token, '/api/v1/system/runtime-health', { cache: 'no-store', signal: controller.signal })
			.then((health) => { setRuntimeHealth(health); setTelemetryError('') })
			.catch((requestError) => {
				if (controller.signal.aborted) return
				setTelemetryError(requestError instanceof Error ? requestError.message : 'Host telemetry could not be loaded')
			})
		return () => controller.abort()
	}, [refreshSignal, token])

	const expectedAgentVersion = runtimeHealth?.compatibility.host_agent_version ?? ''
	const queueByHost = new Map((runtimeHealth?.queue.hosts ?? []).map((item) => [item.host_id, item]))
	const statusByHost = new Map(hosts.map((host) => [host.id, hostReadiness(host, instances.filter((instance) => instance.host_id === host.id), expectedAgentVersion, queueByHost.get(host.id)?.admission_open)]))
	const needsAttention = hosts.filter((host) => statusByHost.get(host.id) !== 'READY').length
	const admissionOpen = hosts.filter((host) => queueByHost.get(host.id)?.admission_open !== false).length
	const selectedHost = hosts.find((host) => host.id === selectedHostID)

	return <section className="section-block first-section hosts-section">
		<div className="section-heading"><div><h2>Hosts</h2><p>Infrastructure readiness and managed workload placement</p></div></div>
		{hosts.length === 0 ? <EmptyState icon={Server} title="No hosts" detail="Enroll a native Host Agent before creating instances." /> : <>
			<div className="host-summary-band" aria-label="Host summary">
				<div><span>Online</span><strong>{hosts.filter((host) => host.status === 'ONLINE').length}/{hosts.length}</strong></div>
				<div><span>Needs attention</span><strong>{needsAttention}</strong></div>
				<div><span>Managed instances</span><strong>{instances.length}</strong></div>
				<div><span>Job admission</span><strong>{runtimeHealth ? `${admissionOpen}/${hosts.length} open` : 'Checking'}</strong></div>
			</div>
			<div className="table-wrap"><table className="provider-table host-table"><thead><tr><th>Host</th><th>Instances</th><th>Host Agent</th><th>Admission</th><th>Status</th></tr></thead><tbody>{hosts.map((host) => {
				const assignedInstances = instances.filter((instance) => instance.host_id === host.id)
				const readiness = statusByHost.get(host.id) ?? 'NEEDS_ATTENTION'
				const admission = queueByHost.get(host.id)?.admission_open
				return <tr key={host.id} className={selectedHostID === host.id ? 'selected' : undefined}>
					<td data-label="Host"><button className="host-row-button" onClick={() => setSelectedHostID(host.id)} aria-label={`Open details for ${host.name}`}><strong>{host.name}</strong><span>{host.hostname} · {host.os} / {host.arch}</span></button></td>
					<td data-label="Instances"><div className="host-cell-value"><strong>{assignedInstances.length}</strong><span className="secondary-text">{assignedInstances.filter((instance) => instance.status === 'RUNNING').length} running</span></div></td>
					<td data-label="Host Agent"><div className="host-cell-value"><strong>{host.agent_version}</strong><span className="secondary-text">{expectedAgentVersion ? host.agent_version === expectedAgentVersion ? 'Compatible' : `Expected ${expectedAgentVersion}` : 'Compatibility checking'}</span></div></td>
					<td data-label="Admission"><div className="host-cell-value"><Status value={admission === false ? 'BLOCKED' : admission === true ? 'OPEN' : 'CHECKING'} label={admission === false ? 'Blocked' : admission === true ? 'Open' : 'Checking'} /></div></td>
					<td data-label="Status"><div className="host-cell-value"><Status value={readiness} label={hostReadinessLabel(readiness)} /><span className="secondary-text">Seen {relativeTime(host.last_seen_at)}</span></div></td>
				</tr>
			})}</tbody></table></div>
			<div className="host-telemetry-note"><Activity size={15} /><span>{telemetryError ? 'Operational telemetry is temporarily unavailable. Identity and heartbeat data remain current.' : 'CPU, memory, and host disk telemetry are not reported by the current Host Agent contract.'}</span></div>
		</>}
		{selectedHost && <HostDetailPanel host={selectedHost} instances={instances.filter((instance) => instance.host_id === selectedHost.id)} operations={operations.filter((operation) => instances.some((instance) => instance.host_id === selectedHost.id && instance.id === operation.instance_id))} expectedAgentVersion={expectedAgentVersion} admissionOpen={queueByHost.get(selectedHost.id)?.admission_open} readiness={statusByHost.get(selectedHost.id) ?? 'NEEDS_ATTENTION'} onClose={() => setSelectedHostID('')} onSelectInstance={onSelectInstance} />}
	</section>
}

function HostDetailPanel({ host, instances, operations, expectedAgentVersion, admissionOpen, readiness, onClose, onSelectInstance }: {
	host: Host
	instances: Instance[]
	operations: Operation[]
	expectedAgentVersion: string
	admissionOpen?: boolean
	readiness: string
	onClose: () => void
	onSelectInstance: (instanceID: string) => void
}) {
	const { dialogRef, onKeyDown } = useDialogAccessibility(onClose)
	const recentOperations = [...operations].sort((left, right) => new Date(right.created_at).getTime() - new Date(left.created_at).getTime()).slice(0, 5)
	return createPortal(<div className="operation-detail-backdrop" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose() }}>
		<aside ref={dialogRef} className="operation-detail-panel host-detail-panel" role="dialog" aria-modal="true" aria-labelledby="host-detail-title" tabIndex={-1} onKeyDown={onKeyDown}>
			<div className="operation-detail-heading"><div><span>Host</span><strong id="host-detail-title">{host.name}</strong></div><button className="icon-button" onClick={onClose} title="Close host details" aria-label="Close host details"><X size={18} /></button></div>
			<div className="operation-detail-status"><span>Infrastructure readiness</span><Status value={readiness} label={hostReadinessLabel(readiness)} /></div>
			<div className="operation-detail-grid">
				<div><span>Machine</span><strong>{host.hostname}</strong></div><div><span>Platform</span><strong>{host.os} / {host.arch}</strong></div>
				<div><span>Host Agent</span><strong>{host.agent_version}</strong></div><div><span>Expected Agent</span><strong>{expectedAgentVersion || 'Checking'}</strong></div>
				<div><span>Last heartbeat</span><strong>{relativeTime(host.last_seen_at)}</strong></div><div><span>Job admission</span><strong>{admissionOpen === false ? 'Blocked' : admissionOpen === true ? 'Open' : 'Checking'}</strong></div>
				<div className="wide"><span>Host ID</span><code>{host.id}</code></div>
			</div>
			<section className="host-detail-section"><div className="host-detail-section-heading"><strong>Managed instances</strong><span>{instances.length}</span></div>{instances.length === 0 ? <p className="host-detail-empty">No instances are assigned to this host.</p> : <div className="host-instance-list">{instances.map((instance) => <button key={instance.id} onClick={() => { onClose(); onSelectInstance(instance.id) }}><span><strong>{instance.name}</strong><small>Hermes {installedHermesVersion(instance)}</small></span><Status value={instanceOperationalHealthStatus(instance)} label={healthStatusLabel(instanceOperationalHealthStatus(instance))} /></button>)}</div>}</section>
			<section className="host-detail-section"><div className="host-detail-section-heading"><strong>Recent activity</strong><span>{recentOperations.length}</span></div>{recentOperations.length === 0 ? <p className="host-detail-empty">No recorded operations for workloads on this host.</p> : <ol className="host-activity-list">{recentOperations.map((operation) => <li key={operation.id}><span className={`operation-timeline-dot ${operation.status.toLowerCase()}`} /><div><strong>{operation.summary}</strong><small>{operationStatusLabel(operation.status)} · {relativeTime(operation.updated_at)}</small></div></li>)}</ol>}</section>
			<div className="host-capacity-disclosure"><strong>Resource capacity telemetry unavailable</strong><span>The current Host Agent does not report CPU, memory, host disk, uptime, or service restart counters. Fleet will not infer these values.</span></div>
		</aside>
	</div>, document.body)
}

function OperationsView({ operations, instances, token, nextCursor, loadingMore, onLoadMore, onChanged }: {
  operations: Operation[]
  instances: Instance[]
	token: string
  nextCursor: string | null
  loadingMore: boolean
  onLoadMore: () => Promise<void>
	onChanged: () => Promise<void>
}) {
	return <OperationsWorkspace operations={operations} instances={instances} token={token} nextCursor={nextCursor} loadingMore={loadingMore} onLoadMore={onLoadMore} onChanged={onChanged} />
}

function OperationsWorkspace({ operations, instances, token, fixedInstanceID = '', pageSize = 10, nextCursor = null, loadingMore = false, onLoadMore, onChanged }: {
	operations: Operation[]
	instances: Instance[]
	token: string
	fixedInstanceID?: string
	pageSize?: number
	nextCursor?: string | null
	loadingMore?: boolean
	onLoadMore?: () => Promise<void>
	onChanged: () => Promise<void>
}) {
  const [query, setQuery] = useState('')
	const [statusFilter, setStatusFilter] = useState(() => !fixedInstanceID && groupOperations(operations).some((group) => ['PENDING', 'RUNNING'].includes(group.status)) ? 'ACTIVE' : 'ALL')
	const [typeFilter, setTypeFilter] = useState('ALL')
	const [instanceFilter, setInstanceFilter] = useState(fixedInstanceID || 'ALL')
	const [timeFilter, setTimeFilter] = useState('ALL')
  const [page, setPage] = useState(0)
	const [selectedGroupID, setSelectedGroupID] = useState('')
  const normalizedQuery = query.trim().toLowerCase()
  const groups = groupOperations(operations)
	const instanceNames = new Map(instances.map((instance) => [instance.id, instance.name]))
  const filtered = groups.filter((group) => {
		const instanceID = group.operations.find((operation) => operation.instance_id)?.instance_id ?? ''
		const searchable = [group.summary, group.type, group.id, group.actor, instanceNames.get(instanceID) ?? '', ...group.operations.flatMap((operation) => [operation.summary, operation.type, operation.id])]
    const matchesQuery = !normalizedQuery || searchable.some((value) => value.toLowerCase().includes(normalizedQuery))
		const matchesStatus = statusFilter === 'ALL' || (statusFilter === 'ACTIVE' ? ['PENDING', 'RUNNING'].includes(group.status) : group.status === statusFilter)
		const matchesType = typeFilter === 'ALL' || group.type === typeFilter
		const matchesInstance = instanceFilter === 'ALL' || instanceID === instanceFilter
		const matchesTime = operationWithinTimeRange(group.createdAt, timeFilter)
		return matchesQuery && matchesStatus && matchesType && matchesInstance && matchesTime
  })
  const pageCount = Math.max(1, Math.ceil(filtered.length / pageSize))
  const safePage = Math.min(page, pageCount - 1)
  const visibleGroups = filtered.slice(safePage * pageSize, (safePage + 1) * pageSize)
	const types = [...new Set(groups.map((group) => group.type))].sort()
	const selectedGroup = groups.find((group) => group.id === selectedGroupID) ?? null
	const activeCount = groups.filter((group) => ['PENDING', 'RUNNING'].includes(group.status)).length
	const failedCount = groups.filter((group) => group.status === 'FAILED').length
	const completedCount = groups.filter((group) => group.status === 'SUCCEEDED').length
	const filtersActive = Boolean(normalizedQuery || statusFilter !== 'ALL' || typeFilter !== 'ALL' || (!fixedInstanceID && instanceFilter !== 'ALL') || timeFilter !== 'ALL')
	const resetFilters = () => {
		setQuery('')
		setStatusFilter('ALL')
		setTypeFilter('ALL')
		setInstanceFilter(fixedInstanceID || 'ALL')
		setTimeFilter('ALL')
		setPage(0)
	}

	return <section className="section-block first-section operations-section">
		<div className="section-heading"><div><h2>Operations</h2><p>{filtersActive ? `${filtered.length} matching of ${groups.length} ${plural(groups.length, 'operation')}` : `${groups.length} ${plural(groups.length, 'operation')} shown`}{nextCursor ? ' · older history available' : ''}</p></div>{filtersActive && <button className="text-button" onClick={resetFilters}>Clear filters</button>}</div>
		<div className="operation-summary-band" aria-label="Operation status summary">
			<div><span>Active</span><strong>{activeCount}</strong></div>
			<div><span>Failed</span><strong>{failedCount}</strong></div>
			<div><span>Completed</span><strong>{completedCount}</strong></div>
		</div>
		<div className="operations-toolbar">
			<input aria-label="Search operations" placeholder="Search summary, ID, actor, or instance" value={query} onChange={(event) => { setQuery(event.target.value); setPage(0) }} />
			{!fixedInstanceID && <select aria-label="Filter operations by instance" value={instanceFilter} onChange={(event) => { setInstanceFilter(event.target.value); setPage(0) }}><option value="ALL">All instances</option>{instances.map((instance) => <option key={instance.id} value={instance.id}>{instance.name}</option>)}</select>}
			<select aria-label="Filter operations by type" value={typeFilter} onChange={(event) => { setTypeFilter(event.target.value); setPage(0) }}><option value="ALL">All types</option>{types.map((type) => <option key={type} value={type}>{operationTypeLabel(type)}</option>)}</select>
			<select aria-label="Filter operations by status" value={statusFilter} onChange={(event) => { setStatusFilter(event.target.value); setPage(0) }}><option value="ALL">All statuses</option><option value="ACTIVE">Active</option>{['PENDING', 'RUNNING', 'FAILED', 'SUCCEEDED'].map((status) => <option key={status} value={status}>{operationStatusLabel(status)}</option>)}</select>
			<select aria-label="Filter operations by time" value={timeFilter} onChange={(event) => { setTimeFilter(event.target.value); setPage(0) }}><option value="ALL">Any time</option><option value="24H">Last 24 hours</option><option value="7D">Last 7 days</option><option value="30D">Last 30 days</option></select>
		</div>
		<div className={`operations-workspace${selectedGroup ? ' has-detail' : ''}`}>
			<div className="operations-list"><OperationsTable groups={visibleGroups} instanceNames={instanceNames} selectedGroupID={selectedGroupID} onSelect={setSelectedGroupID} />{(filtered.length > pageSize || nextCursor) && <div className="pagination"><span>Page {safePage + 1} of {pageCount}{nextCursor ? ' · older history available' : ''}</span><div><button className="secondary-button compact-button" onClick={() => setPage(Math.max(0, safePage - 1))} disabled={safePage === 0}>Previous</button><button className="secondary-button compact-button" onClick={() => setPage(Math.min(pageCount - 1, safePage + 1))} disabled={safePage >= pageCount - 1}>Next</button>{nextCursor && onLoadMore && <button className="secondary-button compact-button" onClick={() => void onLoadMore()} disabled={loadingMore}>{loadingMore ? 'Loading older' : 'Load older'}</button>}</div></div>}</div>
			{selectedGroup && createPortal(<OperationDetailPanel group={selectedGroup} instanceName={instanceNames.get(selectedGroup.operations.find((operation) => operation.instance_id)?.instance_id ?? '') ?? 'Fleet Manager'} token={token} onChanged={onChanged} onClose={() => setSelectedGroupID('')} />, document.body)}
		</div>
	</section>
}

function OperationsTable({ groups, instanceNames, selectedGroupID, onSelect }: { groups: OperationGroup[]; instanceNames: Map<string, string>; selectedGroupID: string; onSelect: (groupID: string) => void }) {
  if (groups.length === 0) return <EmptyState icon={History} title="No operations" detail="Lifecycle operations will appear here." />
  return <div className="table-wrap"><table className="provider-table operations-table"><thead><tr><th>Operation</th><th>Instance</th><th>Type</th><th>Status</th><th>Started / duration</th></tr></thead><tbody>{groups.map((group) => {
		const instanceID = group.operations.find((operation) => operation.instance_id)?.instance_id ?? ''
		const latest = latestOperation(group.operations)
		const context = [operationMetadataSummary(latest), operationProgressSummary(latest)].filter(Boolean).join(' · ')
		return <tr key={group.id} className={selectedGroupID === group.id ? 'selected' : ''}><td data-label="Operation"><button className="operation-row-button" onClick={() => onSelect(group.id)} aria-pressed={selectedGroupID === group.id}><strong>{group.summary}</strong><span>{group.operations.length > 1 ? `${group.operations.length} steps · ` : ''}{group.id.slice(0, 8)}{context ? ` · ${context}` : ''}</span></button></td><td data-label="Instance">{instanceNames.get(instanceID) ?? 'Fleet Manager'}</td><td data-label="Type">{operationTypeLabel(group.type)}</td><td data-label="Status"><Status value={group.status} label={operationStatusLabel(group.status)} /><span className="secondary-text">{operationActorLabel(group.actor)}</span></td><td data-label="Started / duration">{relativeTime(group.createdAt)}<span className="secondary-text">Duration {operationDuration(group.createdAt, group.updatedAt, group.status)}</span></td></tr>
	})}</tbody></table></div>
}

function OperationDetailPanel({ group, instanceName, token, onChanged, onClose }: { group: OperationGroup; instanceName: string; token: string; onChanged: () => Promise<void>; onClose: () => void }) {
	const latest = latestOperation(group.operations)
	const sessionID = group.type === 'CHAT_MESSAGE' && ['PENDING', 'RUNNING'].includes(group.status) && typeof latest.metadata?.chat_session_id === 'string'
		? latest.metadata.chat_session_id
		: ''
	const [stopping, setStopping] = useState(false)
	const [stopError, setStopError] = useState('')
	const { dialogRef, onKeyDown } = useDialogAccessibility(onClose)
	const stopChatResponse = async () => {
		if (!sessionID || stopping) return
		setStopping(true)
		setStopError('')
		try {
			await apiRequest<void>(token, `/api/v1/chats/${sessionID}/cancel`, { method: 'POST', body: '{}' })
			await onChanged()
		} catch (requestError) {
			setStopError(requestError instanceof Error ? requestError.message : 'Chat response could not be stopped')
		} finally {
			setStopping(false)
		}
	}
	return <div className="operation-detail-backdrop" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose() }}><aside ref={dialogRef} className="operation-detail-panel" role="dialog" aria-modal="true" aria-label="Operation details" tabIndex={-1} onKeyDown={onKeyDown}>
		<div className="operation-detail-heading"><div><span>Operation details</span><strong>{group.summary}</strong></div><div className="operation-detail-heading-actions">{sessionID && <button className="secondary-button compact-button danger-button" onClick={() => void stopChatResponse()} disabled={stopping}><CircleStop size={15} />{stopping ? 'Stopping' : 'Stop response'}</button>}<button className="icon-button" onClick={onClose} aria-label="Close operation details"><X size={16} /></button></div></div>
		<div className="operation-detail-status"><Status value={group.status} label={operationStatusLabel(group.status)} /><span>{instanceName}</span></div>
		<div className="operation-detail-grid">
			<div><span>Type</span><strong>{operationTypeLabel(group.type)}</strong></div>
			<div><span>Actor</span><strong>{operationActorLabel(group.actor)}</strong></div>
			<div><span>Started</span><strong>{relativeTime(group.createdAt)}</strong></div>
			<div><span>Duration</span><strong>{operationDuration(group.createdAt, group.updatedAt, group.status)}</strong></div>
			<div className="wide"><span>Workflow ID</span><code>{group.id}</code></div>
		</div>
		{latest.progress?.steps && latest.progress.steps.length > 0 && <div className="operation-stage-section"><strong>Progress</strong><ol>{latest.progress.steps.map((step) => <li key={step.stage} className={step.status}><span aria-hidden="true">{step.status === 'succeeded' ? '✓' : step.status === 'failed' ? '✕' : step.status === 'running' ? '●' : '○'}</span><div><strong>{operationStageLabel(step.stage)}</strong>{step.detail && <small>{step.detail}</small>}</div></li>)}</ol></div>}
		<div className="operation-detail-timeline"><strong>Audit timeline</strong><OperationTimeline operations={group.operations} hideErrorForOperationID={latest.id} /></div>
		{(latest.error || latest.progress?.detail) && <div className="operation-detail-error"><strong>{latest.status === 'FAILED' ? 'Failure detail' : 'Current detail'}</strong><span>{latest.error || latest.progress?.detail}</span>{latest.progress?.action_code && <small>Required action: {sentenceCase(latest.progress.action_code.replaceAll('_', ' '))}. Return to the originating module to retry safely.</small>}</div>}
		{stopError && <div className="operation-detail-error"><strong>Stop failed</strong><span>{stopError}</span></div>}
	</aside></div>
}

function OperationTimeline({ operations, hideErrorForOperationID = '' }: { operations: Operation[]; hideErrorForOperationID?: string }) {
	const ordered = [...operations].sort((left, right) => new Date(left.created_at).getTime() - new Date(right.created_at).getTime())
	return <ol className="operation-timeline">{ordered.map((operation) => {
		const context = [operationMetadataSummary(operation), operationProgressSummary(operation)].filter(Boolean).join(' · ')
		return <li key={operation.id}><span className={`operation-timeline-dot ${operation.status.toLowerCase()}`} aria-hidden="true" /><div><strong>{operationDisplayType(operation)}</strong><small>{relativeTime(operation.created_at)} · {operationDuration(operation.created_at, operation.updated_at, operation.status)}{context ? ` · ${context}` : ''}</small>{operation.error && operation.id !== hideErrorForOperationID && <span className="operation-timeline-error">{operation.error}</span>}</div><Status value={operation.status} label={operationStatusLabel(operation.status)} /></li>
	})}</ol>
}

function groupOperations(operations: Operation[]): OperationGroup[] {
	const grouped = new Map<string, Operation[]>()
	for (const operation of operations) {
		const key = operation.workflow_id || operation.id
		grouped.set(key, [...(grouped.get(key) ?? []), operation])
	}
	return [...grouped.entries()].map(([id, items]) => {
		const ordered = [...items].sort((left, right) => new Date(left.created_at).getTime() - new Date(right.created_at).getTime())
		const update = ordered.find((operation) => operation.type === 'UPGRADE_HERMES')
		const status = operationGroupStatus(ordered)
		return {
			id,
			operations: ordered,
			summary: update?.summary ?? ordered[0].summary,
			type: update
				? update.metadata?.update_kind === 'RUNTIME_REFRESH' ? 'HERMES_RUNTIME_REFRESH_WORKFLOW' : 'HERMES_UPDATE_WORKFLOW'
				: ordered[0].type,
			status,
			actor: ordered[0].actor || 'FLEET_ADMIN',
			createdAt: ordered[0].created_at,
			updatedAt: ordered.reduce((latest, operation) => new Date(operation.updated_at) > new Date(latest) ? operation.updated_at : latest, ordered[0].updated_at),
		}
	}).sort((left, right) => new Date(right.createdAt).getTime() - new Date(left.createdAt).getTime())
}

function latestOperation(operations: Operation[]) {
	return [...operations].sort((left, right) => {
		const updatedDifference = new Date(right.updated_at).getTime() - new Date(left.updated_at).getTime()
		return updatedDifference || new Date(right.created_at).getTime() - new Date(left.created_at).getTime()
	})[0]
}

function operationWithinTimeRange(createdAt: string, range: string) {
	if (range === 'ALL') return true
	const duration = range === '24H' ? 24 * 60 * 60 * 1000 : range === '7D' ? 7 * 24 * 60 * 60 * 1000 : 30 * 24 * 60 * 60 * 1000
	return Date.now() - new Date(createdAt).getTime() <= duration
}

function operationStageLabel(stage: string) {
	return sentenceCase(stage.replaceAll('_', ' '))
}

function operationGroupStatus(operations: Operation[]) {
	const latest = operations.reduce<Operation | null>((current, operation) => {
		if (!current) return operation
		const currentAttempt = typeof current.metadata?.attempt === 'number' ? current.metadata.attempt : 0
		const operationAttempt = typeof operation.metadata?.attempt === 'number' ? operation.metadata.attempt : 0
		if (operationAttempt !== currentAttempt) return operationAttempt > currentAttempt ? operation : current
		const createdDifference = new Date(operation.created_at).getTime() - new Date(current.created_at).getTime()
		if (createdDifference !== 0) return createdDifference > 0 ? operation : current
		const updatedDifference = new Date(operation.updated_at).getTime() - new Date(current.updated_at).getTime()
		if (updatedDifference !== 0) return updatedDifference > 0 ? operation : current
		return operation.id.localeCompare(current.id) > 0 ? operation : current
	}, null)
	return latest?.status ?? 'PENDING'
}

function operationMetadataSummary(operation: Operation) {
	const metadata = operation.metadata
	if (!metadata) return ''
	const parts: string[] = []
	if (typeof metadata.from_version === 'string' && typeof metadata.to_version === 'string') {
		if (metadata.update_kind === 'RUNTIME_REFRESH' && metadata.from_version === metadata.to_version) parts.push(`Hermes ${metadata.to_version} unchanged`)
		else parts.push(`${metadata.from_version} → ${metadata.to_version}`)
	}
	if (typeof metadata.backup_id === 'string') parts.push(`backup ${compactResourceID(metadata.backup_id)}`)
	if (typeof metadata.image_digest === 'string') parts.push(`image ${metadata.image_digest.slice(0, 15)}…`)
	if (typeof metadata.attempt === 'number' && metadata.attempt > 1) parts.push(`attempt ${metadata.attempt}`)
	return parts.join(' · ')
}

function operationProgressSummary(operation: Operation) {
	if (!operation.progress?.stage || !['PENDING', 'RUNNING'].includes(operation.status)) return ''
	const runtimeRefresh = operation.metadata?.update_kind === 'RUNTIME_REFRESH'
	const labels: Record<string, string> = {
		PREPARING_RELEASE: runtimeRefresh ? 'preparing managed runtime' : 'preparing release',
		STOPPING: 'stopping instance',
		BACKING_UP: 'verifying backup',
		INSTALLING: runtimeRefresh ? 'refreshing managed runtime' : 'installing Hermes',
		RESTORING_STATE: 'restoring runtime state',
		VERIFYING_VERSION: runtimeRefresh ? 'verifying Hermes' : 'verifying installed version',
	}
	return labels[operation.progress.stage] ? `current step: ${labels[operation.progress.stage]}` : ''
}

function operationDisplayType(operation: Operation) {
	if (operation.type === 'UPGRADE_HERMES' && operation.metadata?.update_kind === 'RUNTIME_REFRESH') {
		return 'Refresh managed runtime'
	}
	return operationTypeLabel(operation.type)
}

function compactResourceID(value: string) {
	const suffix = value.includes('-') ? value.slice(value.indexOf('-') + 1) : value
	return suffix.slice(0, 8)
}

function SystemView({ token, refreshSignal }: { token: string; refreshSignal: number }) {
	const [section, setSection] = useState<SystemSection>(() => systemSectionFromHash())
	const [systemInfo, setSystemInfo] = useState<SystemInfo | null>(null)
	const [loading, setLoading] = useState(true)
	const [error, setError] = useState('')
	const loadController = useRef<AbortController | null>(null)
	const loadSequence = useRef(0)
	const sectionButtons = useRef<Partial<Record<SystemSection, HTMLButtonElement | null>>>({})

	const loadSystemInfo = useCallback(async () => {
		const sequence = loadSequence.current + 1
		loadSequence.current = sequence
		loadController.current?.abort()
		const controller = new AbortController()
		loadController.current = controller
		setLoading(true)
		setError('')
		try {
			const info = await apiRequest<SystemInfo>(token, '/api/v1/system', {
				cache: 'no-store',
				signal: controller.signal,
			})
			if (!controller.signal.aborted && sequence === loadSequence.current) setSystemInfo(info)
		} catch (requestError) {
			if (requestError instanceof DOMException && requestError.name === 'AbortError') return
			if (sequence === loadSequence.current) setError(requestError instanceof Error ? requestError.message : 'Could not load system information')
		} finally {
			if (sequence === loadSequence.current) {
				if (loadController.current === controller) loadController.current = null
				setLoading(false)
			}
		}
	}, [token])

	useEffect(() => {
		const onHistoryChange = () => setSection(systemSectionFromHash())
		window.addEventListener('popstate', onHistoryChange)
		if (!['#system/general', '#system/runtime-health', '#system/remote-access', '#system/backups'].includes(window.location.hash)) {
			window.history.replaceState(null, '', '#system/general')
		}
		return () => {
			window.removeEventListener('popstate', onHistoryChange)
		}
	}, [])

	useEffect(() => {
		const initial = window.setTimeout(() => void loadSystemInfo(), 0)
		return () => {
			window.clearTimeout(initial)
			loadController.current?.abort()
		}
	}, [loadSystemInfo, refreshSignal])

	useEffect(() => {
		if (systemInfo?.recovery_drill?.status !== 'RUNNING') return
		const timer = window.setInterval(() => void loadSystemInfo(), 2000)
		return () => window.clearInterval(timer)
	}, [loadSystemInfo, systemInfo?.recovery_drill?.status])

	useEffect(() => {
		sectionButtons.current[section]?.scrollIntoView({ block: 'nearest', inline: 'center' })
	}, [section])

	const navigate = (next: SystemSection) => {
		setSection(next)
		window.history.pushState(null, '', `#system/${next}`)
	}
	const sections: { id: SystemSection; label: string }[] = [
		{ id: 'general', label: 'Overview' },
		{ id: 'runtime-health', label: 'Runtime health' },
		{ id: 'remote-access', label: 'Remote access' },
		{ id: 'backups', label: 'Backups & recovery' },
	]

	return <div className="system-layout">
		<nav className="instance-tabs system-tabs" aria-label="System modules">
			{sections.map((item) => <button ref={(element) => { sectionButtons.current[item.id] = element }} key={item.id} aria-current={section === item.id ? 'page' : undefined} onClick={() => navigate(item.id)}>{item.label}</button>)}
		</nav>
		{error && <div className="inline-error system-error">{error}</div>}
		{section === 'general' && <SystemGeneralPanel info={systemInfo} loading={loading} />}
		{section === 'runtime-health' && <RuntimeHealthPanel token={token} info={systemInfo} />}
		{section === 'remote-access' && <RemoteAccessPanel token={token} info={systemInfo} loading={loading} reload={loadSystemInfo} />}
		{section === 'backups' && <BackupsPanel token={token} refreshSignal={refreshSignal} info={systemInfo} systemLoading={loading} reloadSystemInfo={loadSystemInfo} />}
	</div>
}

function RuntimeHealthPanel({ token, info }: { token: string; info: SystemInfo | null }) {
	const [health, setHealth] = useState<RuntimeHealth | null>(null)
	const [loading, setLoading] = useState(true)
	const [error, setError] = useState('')
	const load = useCallback(async () => {
		setLoading(true)
		try {
			setHealth(await apiRequest<RuntimeHealth>(token, '/api/v1/system/runtime-health', { cache: 'no-store' }))
			setError('')
		} catch (requestError) {
			setError(requestError instanceof Error ? requestError.message : 'Runtime health could not be loaded')
		} finally {
			setLoading(false)
		}
	}, [token])
	useEffect(() => {
		const initial = window.setTimeout(() => void load(), 0)
		const timer = window.setInterval(() => void load(), 10000)
		return () => {
			window.clearTimeout(initial)
			window.clearInterval(timer)
		}
	}, [load])
	const manifest = info?.capabilities
	const queue = health?.queue
	const metrics = health?.metrics
	const degradedComponents = health?.components.filter((component) => component.status === 'degraded') || []
	const componentLabel = (component: string) => ({ control_plane: 'Control plane', host_queue: 'Host work queue', remote_access: 'Remote access' }[component] || component)
	return <section className="section-block first-section">
		<div className="section-heading"><div><h2>Runtime health</h2><p>Current operational state and actionable failures</p></div><div className="button-row">{health && <Status value={health.status === 'healthy' ? 'READY' : 'FAILED'} label={health.status === 'healthy' ? 'Healthy' : 'Needs attention'} />}<button className="secondary-button compact-button" onClick={() => void load()} disabled={loading}><RefreshCw className={loading ? 'spin' : ''} size={15} />Refresh</button></div></div>
		<div className={`runtime-health-summary${degradedComponents.length ? ' degraded' : ''}`}>
			<Activity size={18} />
			<div><strong>{loading && !health ? 'Checking Fleet runtime' : degradedComponents.length ? `${degradedComponents.length} ${plural(degradedComponents.length, 'component')} need attention` : 'All monitored components are healthy'}</strong><span>{degradedComponents.length ? degradedComponents.map((component) => componentLabel(component.component)).join(', ') : 'Control plane, Host work queue, and Remote access passed their latest checks.'}</span></div>
		</div>
		<details className="diagnostics-details runtime-technical-details">
			<summary>Technical details</summary>
			<div className="settings-grid">
			<div className="settings-row"><div><strong>Live state updates</strong><span>UI refreshes after authoritative state revisions</span></div><div><strong>{health ? `Revision ${health.state_revision}` : 'Loading'}</strong>{health && <span>{health.event_subscribers} connected {plural(health.event_subscribers, 'client')}</span>}</div></div>
			<div className="settings-row"><div><strong>Host work queue</strong><span>Pending and leased Host Agent work</span></div><div><strong>{queue ? `${queue.pending} pending · ${queue.active} active` : 'Loading'}</strong>{queue && <span>{queue.expired_leases ? `${queue.expired_leases} expired leases need reconciliation` : `Admission open below ${queue.max_per_host} jobs per host`}</span>}</div></div>
			<div className="settings-row"><div><strong>Request processing</strong><span>{metrics ? `Latency uses the latest ${metrics.duration_samples} API requests; totals reset when Fleet Manager restarts` : 'Loading the current request-latency window'}</span></div><div><strong>{metrics ? `${metrics.p95_http_ms.toFixed(1)} ms p95 · ${metrics.http_requests} total requests` : 'Loading'}</strong>{metrics && <span>{metrics.average_http_ms.toFixed(1)} ms average · {metrics.p99_http_ms.toFixed(1)} ms p99 · {metrics.max_http_ms.toFixed(1)} ms max · {metrics.http_failures} server failures</span>}</div></div>
			<div className="settings-row"><div><strong>Compatibility contract</strong><span>Versions and runtime configuration schemas accepted by this Fleet release</span></div><div><strong>{manifest ? `Host Agent ${manifest.host_agent_version}` : 'Loading'}</strong>{manifest && <span>Supported schemas {manifest.runtime_config_schemas.join(', ')} · worker concurrency default {manifest.default_job_concurrency}, maximum {manifest.maximum_job_concurrency}</span>}</div></div>
			<div className="settings-row"><div><strong>Persistent health history</strong><span>Component transitions survive Fleet Manager restarts</span></div><div><strong>{health ? degradedComponents.length ? `${degradedComponents.length} degraded` : `${health.components.length} components healthy` : 'Loading'}</strong>{health?.components.map((component) => <span key={component.component}>{componentLabel(component.component)}: {component.last_success_at ? `last healthy ${relativeTime(component.last_success_at)}` : 'not yet healthy'}</span>)}</div></div>
			</div>
			{health && health.recent_incidents.length > 0 && <div className="runtime-history"><strong>Recent health transitions</strong><div className="settings-grid">{health.recent_incidents.slice(0, 5).map((incident) => <div className="settings-row" key={incident.id}><div><strong>{componentLabel(incident.component)}</strong><span>{incident.detail}</span></div><div><Status value={incident.status === 'healthy' ? 'READY' : 'FAILED'} label={incident.status === 'healthy' ? 'Recovered' : 'Degraded'} /><span>{relativeTime(incident.occurred_at)}</span></div></div>)}</div></div>}
		</details>
		{error && <div className="inline-error">{error}</div>}
	</section>
}

function RemoteAccessPanel({ token, info, loading, reload }: { token: string; info: SystemInfo | null; loading: boolean; reload: () => Promise<void> }) {
	const [syncing, setSyncing] = useState(false)
	const [savingTarget, setSavingTarget] = useState<'admin' | 'publishing' | 'endpoints' | ''>('')
	const [syncError, setSyncError] = useState('')
	const [editing, setEditing] = useState(false)
	const [configuration, setConfiguration] = useState<RemoteAccessConfiguration | null>(null)
	const [instances, setInstances] = useState<Instance[]>([])
	const [replacingToken, setReplacingToken] = useState<'admin' | 'instances' | 'api' | ''>('')
	const [editingAdminHostname, setEditingAdminHostname] = useState(false)
	const [mode, setMode] = useState<RemoteAccessMode | ''>('')
	const [adminURL, setAdminURL] = useState('')
	const [instanceURLs, setInstanceURLs] = useState<Record<string, string>>({})
	const [form, setForm] = useState({
		admin_tunnel_token: '', instances_tunnel_token: '', admin_hostname: '',
		route_account_id: '', route_zone_id: '', route_api_token: '', route_fleet_namespace: '',
	})
	const dirtyFormFields = useRef(new Set<keyof typeof form>())
	const remote = info?.remote_access
	const loadConfiguration = useCallback(async () => {
		try {
			const [configurationResult, instancesResult] = await Promise.allSettled([
				apiRequest<RemoteAccessConfiguration>(token, '/api/v1/system/remote-access/configuration', { cache: 'no-store' }),
				apiRequest<Instance[] | null>(token, '/api/v1/instances', { cache: 'no-store' }),
			])
			if (configurationResult.status === 'rejected') throw configurationResult.reason
			const value = configurationResult.value
			setConfiguration(value)
			if (instancesResult.status === 'fulfilled') {
				setInstances((instancesResult.value ?? []).filter((instance) => instance.status !== 'DELETED'))
				setSyncError('')
			} else {
				setInstances([])
				setSyncError(value.mode === 'existing_endpoints' ? 'Instance endpoints cannot be edited because the instance list could not be loaded.' : '')
			}
			setMode(value.mode || '')
			setAdminURL(value.admin_url || '')
			setInstanceURLs(Object.fromEntries((value.instance_endpoints ?? []).map((endpoint) => [endpoint.instance_id, endpoint.dashboard_url || ''])))
			setForm((current) => ({
				...current,
				admin_tunnel_token: dirtyFormFields.current.has('admin_tunnel_token') ? current.admin_tunnel_token : '',
				instances_tunnel_token: dirtyFormFields.current.has('instances_tunnel_token') ? current.instances_tunnel_token : '',
				route_api_token: dirtyFormFields.current.has('route_api_token') ? current.route_api_token : '',
				admin_hostname: dirtyFormFields.current.has('admin_hostname') ? current.admin_hostname : value.admin_hostname || '',
				route_account_id: dirtyFormFields.current.has('route_account_id') ? current.route_account_id : value.instance_publishing_account_id || '',
				route_zone_id: dirtyFormFields.current.has('route_zone_id') ? current.route_zone_id : value.instance_publishing_zone_id || '',
				route_fleet_namespace: dirtyFormFields.current.has('route_fleet_namespace') ? current.route_fleet_namespace : value.instance_publishing_fleet_namespace || '',
			}))
		} catch (requestError) {
			setSyncError(requestError instanceof Error ? requestError.message : 'Remote access configuration could not be loaded')
		}
	}, [token])
	useEffect(() => {
		const initial = window.setTimeout(() => void loadConfiguration(), 0)
		return () => window.clearTimeout(initial)
	}, [loadConfiguration, remote?.configured])
	useEffect(() => {
		if (!remote?.configured || !['pending', 'syncing'].includes(remote.state)) return
		const timer = window.setInterval(() => void Promise.all([reload(), loadConfiguration()]), 2000)
		return () => window.clearInterval(timer)
	}, [loadConfiguration, reload, remote?.configured, remote?.state])
	const updateField = (field: keyof typeof form, value: string) => {
		dirtyFormFields.current.add(field)
		setForm((current) => ({ ...current, [field]: value }))
	}
	const updateInstanceURL = (instanceID: string, value: string) => setInstanceURLs((current) => ({ ...current, [instanceID]: value }))
	const tunnelTokenField = (boundary: 'admin' | 'instances', configured: boolean | undefined) => {
		const field = boundary === 'admin' ? 'admin_tunnel_token' : 'instances_tunnel_token'
		const fingerprint = boundary === 'admin'
			? configuration?.admin_tunnel_token_fingerprint
			: configuration?.instances_tunnel_token_fingerprint
		if (configured && replacingToken !== boundary) {
			return <div className="remote-secret-field"><div className="messaging-secret-summary"><div><span>Tunnel token</span><code aria-label="Stored tunnel token">{fingerprint ? `ID ${fingerprint}` : 'Stored'}</code><small>Non-secret fingerprint · Stored encrypted by Fleet</small></div><button type="button" className="secondary-button" onClick={() => setReplacingToken(boundary)} disabled={syncing}>Replace token</button></div></div>
		}
		return <div className="remote-token-editor"><label>{configured ? 'New tunnel token' : 'Tunnel token'}<input type="text" required={!configured} autoComplete="off" autoCapitalize="none" spellCheck={false} value={form[field]} onChange={(event) => updateField(field, event.target.value)} /></label>{configured && <button type="button" className="text-button" onClick={() => { updateField(field, ''); setReplacingToken('') }} disabled={syncing}>Cancel replacement</button>}</div>
	}
	const publishingAPITokenField = () => {
		if (configuration?.instance_publishing_configured && replacingToken !== 'api') {
			return <div className="remote-secret-field"><div className="messaging-secret-summary"><div><span>Cloudflare API token</span><code aria-label="Stored Cloudflare API token">{configuration.instance_publishing_token_fingerprint ? `ID ${configuration.instance_publishing_token_fingerprint}` : 'Stored'}</code><small>Non-secret fingerprint · Stored encrypted by Fleet</small></div><button type="button" className="secondary-button" onClick={() => setReplacingToken('api')} disabled={syncing}>Replace token</button></div></div>
		}
		return <div className="remote-token-editor"><label>{configuration?.instance_publishing_configured ? 'New Cloudflare API token' : 'Cloudflare API token'}<input type="text" required={!configuration?.instance_publishing_configured} autoComplete="off" autoCapitalize="none" spellCheck={false} value={form.route_api_token} onChange={(event) => updateField('route_api_token', event.target.value)} /></label>{configuration?.instance_publishing_configured && <button type="button" className="text-button" onClick={() => { updateField('route_api_token', ''); setReplacingToken('') }} disabled={syncing}>Cancel replacement</button>}</div>
	}
	const adminHostnameField = () => {
		const storedHostname = configuration?.admin_hostname?.trim() || ''
		if (storedHostname && !editingAdminHostname) {
			return <div className="remote-summary-field"><div className="messaging-secret-summary"><div><span>Public hostname</span><code aria-label="Saved public hostname">{storedHostname}</code><small>Active admin route · Saved by Fleet</small></div><button type="button" className="secondary-button" onClick={() => setEditingAdminHostname(true)} disabled={syncing}>Change hostname</button></div></div>
		}
		return <div className="remote-token-editor"><label>Public hostname<input required placeholder="admin.example.com" value={form.admin_hostname} onChange={(event) => updateField('admin_hostname', event.target.value)} /><span>The hostname configured in the Cloudflare published application route.</span></label>{storedHostname && <button type="button" className="text-button" onClick={() => { updateField('admin_hostname', storedHostname); setEditingAdminHostname(false) }} disabled={syncing}>Cancel hostname change</button>}</div>
	}
	const saveAdminBoundary = async (event: FormEvent) => {
		event.preventDefault()
		setSyncing(true)
		setSavingTarget('admin')
		setSyncError('')
		try {
			await apiRequest(token, '/api/v1/system/remote-access/cloudflare/admin', { method: 'PUT', body: JSON.stringify({ tunnel_token: form.admin_tunnel_token, hostname: form.admin_hostname }) })
			dirtyFormFields.current.delete('admin_tunnel_token')
			dirtyFormFields.current.delete('admin_hostname')
			await Promise.all([reload(), loadConfiguration()])
			setForm((current) => ({ ...current, admin_tunnel_token: '' }))
			setReplacingToken('')
			setEditingAdminHostname(false)
		} catch (requestError) {
			setSyncError(requestError instanceof Error ? requestError.message : 'Admin tunnel could not be applied')
		} finally {
			setSyncing(false)
			setSavingTarget('')
		}
	}
	const saveEndpoints = async (event: FormEvent) => {
		event.preventDefault()
		setSyncing(true)
		setSavingTarget('endpoints')
		setSyncError('')
		try {
			await apiRequest(token, '/api/v1/system/remote-access/configuration', {
				method: 'PUT',
				body: JSON.stringify({
					mode: 'existing_endpoints', admin_url: adminURL,
					instance_endpoints: instances.map((instance) => ({
						instance_id: instance.id, instance_name: instance.name,
						dashboard_url: instanceURLs[instance.id] || '',
					})),
				}),
			})
			await Promise.all([reload(), loadConfiguration()])
			setEditing(false)
		} catch (requestError) {
			setSyncError(requestError instanceof Error ? requestError.message : 'Public endpoints could not be saved')
		} finally {
			setSyncing(false)
			setSavingTarget('')
		}
	}
	const saveInstancePublishing = async (event: FormEvent) => {
		event.preventDefault()
		setSyncing(true)
		setSavingTarget('publishing')
		setSyncError('')
		try {
			let operation = await apiRequest<Operation>(token, '/api/v1/system/remote-access/cloudflare/instance-publishing', {
				method: 'PUT',
				body: JSON.stringify({
					tunnel_token: form.instances_tunnel_token,
					account_id: form.route_account_id,
					zone_id: form.route_zone_id,
					api_token: form.route_api_token,
					fleet_namespace: form.route_fleet_namespace,
				}),
			})
			const controller = new AbortController()
			operation = await waitForOperationResult(token, operation.id, controller.signal)
			if (operation.status === 'FAILED') {
				throw new Error(operation.progress?.detail || operation.error || 'Instance publishing verification failed')
			}
			dirtyFormFields.current.delete('instances_tunnel_token')
			dirtyFormFields.current.delete('route_account_id')
			dirtyFormFields.current.delete('route_zone_id')
			dirtyFormFields.current.delete('route_api_token')
			dirtyFormFields.current.delete('route_fleet_namespace')
			await Promise.all([reload(), loadConfiguration()])
			setForm((current) => ({ ...current, instances_tunnel_token: '', route_api_token: '' }))
			setReplacingToken('')
			setEditing(false)
		} catch (requestError) {
			setSyncError(requestError instanceof Error ? requestError.message : 'Instance publishing could not be connected')
		} finally {
			setSyncing(false)
			setSavingTarget('')
		}
	}
	const disable = async () => {
		const retryingCleanup = remote?.state === 'cleanup_pending'
		const existingEndpoints = remote?.mode === 'existing_endpoints'
		const confirmation = retryingCleanup
			? configuration?.legacy_provider_managed
				? 'Retry removing the remaining routes and DNS records created by the legacy Fleet configuration?'
				: 'Retry removing the remaining Fleet-managed local ingress files?'
			: existingEndpoints
				? 'Remove the registered public endpoints from Fleet? Your external provider configuration will not be changed.'
				: 'Disable Cloudflare connectors and remove their local token files? Cloudflare tunnel routes, DNS, and Access resources will remain unchanged.'
		if (!window.confirm(confirmation)) return
		setSyncing(true)
		setSyncError('')
		try {
			await apiRequest(token, '/api/v1/system/remote-access/configuration', { method: 'DELETE' })
			await Promise.all([reload(), loadConfiguration()])
			setEditing(true)
		} catch (requestError) {
			setSyncError(requestError instanceof Error ? requestError.message : 'Remote access could not be disabled')
			await Promise.allSettled([reload(), loadConfiguration()])
		} finally {
			setSyncing(false)
		}
	}
	const reconcile = async () => {
		setSyncing(true)
		setSyncError('')
		try {
			await apiRequest(token, '/api/v1/system/remote-access/reconcile', { method: 'POST' })
			await Promise.all([reload(), loadConfiguration()])
		} catch (requestError) {
			setSyncError(requestError instanceof Error ? requestError.message : 'Cloudflare connectors could not be checked')
		} finally {
			setSyncing(false)
		}
	}
	const cleanupPending = remote?.state === 'cleanup_pending'
	const existingEndpoints = remote?.mode === 'existing_endpoints'
	const stateLabel = remote?.state === 'registered' ? 'Endpoints registered' : remote?.state === 'synced' ? 'Connectors ready' : remote?.state === 'pending' ? 'Setup incomplete' : remote?.state === 'syncing' ? 'Starting connectors' : cleanupPending ? 'Cleanup incomplete' : remote?.state === 'error' || remote?.state === 'degraded' ? 'Needs attention' : remote?.configured ? 'Starting connectors' : 'Not configured'
	const stateValue = remote?.state === 'registered' ? 'CONFIGURED' : remote?.state === 'synced' ? 'IN_SYNC' : remote?.state === 'error' || remote?.state === 'degraded' || cleanupPending ? 'FAILED' : remote?.state === 'syncing' ? 'UNKNOWN' : 'STOPPED'
	const adminConnectorReady = remote?.admin.connector_state === 'running'
	const adminEndpointReady = remote?.admin.endpoint_state === 'reachable' || remote?.admin.endpoint_state === 'access_protected'
	const adminEndpointFailed = remote?.admin.endpoint_state === 'unavailable'
	const adminStatusValue = !configuration?.admin_tunnel_token_configured ? 'STOPPED' : adminConnectorReady && adminEndpointReady ? 'READY' : remote?.admin.connector_state === 'unreachable' || adminEndpointFailed ? 'FAILED' : 'UNKNOWN'
	const adminStatusLabel = !configuration?.admin_tunnel_token_configured ? 'Not configured' : adminConnectorReady && adminEndpointReady ? 'Online' : adminEndpointFailed ? 'Endpoint unavailable' : adminConnectorReady ? 'Checking endpoint' : 'Checking connector'
	const managedIncomplete = mode === 'managed_cloudflare' && (!configuration?.admin_tunnel_token_configured || !configuration?.instance_publishing_configured)
	const showEditor = editing || remote?.configured === false || managedIncomplete
	const instanceRoutes = configuration?.instance_routes ?? []
	const namespaceLocked = Boolean(configuration?.instance_publishing_fleet_namespace && instanceRoutes.some((route) => route.hostname))
	const routeHasIssue = instanceRoutes.some((route) =>
		route.dns_state === 'failed' || route.dns_state === 'conflict' ||
		route.route_state === 'failed' || route.route_state === 'conflict' ||
		route.endpoint_state === 'unavailable' || route.endpoint_state === 'access_protected',
	)
	const showPublishingWorkflow = syncing || remote?.state === 'syncing' || routeHasIssue
	const publishingWorkflow = <ol className="remote-route-workflow" aria-label="Cloudflare route publication workflow"><li>Validate hostname</li><li>Create or update DNS</li><li>Create or update tunnel ingress</li><li>Verify Cloudflare configuration</li><li>Check public endpoint</li></ol>
	const routeInventory = <div className="remote-route-inventory">
		<div className="remote-origin-row"><div><strong>Instance publishing inventory</strong><span>Fleet generates each hostname from its namespace and instance name.</span></div><strong className="remote-origin-value">{instanceRoutes.length} managed {plural(instanceRoutes.length, 'instance')}</strong></div>
		{showPublishingWorkflow ? <div className="remote-route-workflow-active"><strong>{routeHasIssue ? 'Publishing workflow needs attention' : 'Publishing workflow in progress'}</strong>{publishingWorkflow}</div> : <details className="remote-route-workflow-details"><summary>How publishing works</summary>{publishingWorkflow}</details>}
		{instanceRoutes.length === 0 ? <div className="empty-state compact-empty"><strong>No managed instances</strong><span>Create an instance before publishing a dashboard.</span></div> : <div className="table-wrap"><table className="provider-table remote-route-table"><thead><tr><th>Instance</th><th>Public hostname</th><th>DNS</th><th>Route</th><th>Endpoint</th></tr></thead><tbody>{instanceRoutes.map((route) => <tr key={route.instance_id}><td data-label="Instance"><strong>{route.instance_name}</strong></td><td data-label="Public hostname"><code>{route.hostname || 'Not configured'}</code></td><td data-label="DNS" title={route.dns_detail || undefined} aria-label={route.dns_detail ? `${remoteResourceStateLabel(route.dns_state)}. ${route.dns_detail}` : undefined}><Status value={route.dns_state === 'ready' ? 'READY' : route.dns_state === 'failed' || route.dns_state === 'conflict' ? 'FAILED' : 'UNKNOWN'} label={route.hostname ? remoteResourceStateLabel(route.dns_state) : '—'} />{route.dns_detail && route.dns_state !== 'ready' && <span className="secondary-text">{route.dns_detail}</span>}</td><td data-label="Route" title={route.route_detail || undefined} aria-label={route.route_detail ? `${remoteResourceStateLabel(route.route_state)}. ${route.route_detail}` : undefined}><Status value={route.route_state === 'ready' ? 'READY' : route.route_state === 'failed' || route.route_state === 'conflict' ? 'FAILED' : 'UNKNOWN'} label={route.hostname ? remoteResourceStateLabel(route.route_state) : '—'} />{route.route_detail && route.route_state !== 'ready' && <span className="secondary-text">{route.route_detail}</span>}</td><td data-label="Endpoint" title={route.endpoint_detail || undefined} aria-label={route.endpoint_detail ? `${remoteRouteEndpointLabel(route.endpoint_state)}. ${route.endpoint_detail}` : undefined}><Status value={route.endpoint_state === 'reachable' ? 'READY' : route.endpoint_state === 'unavailable' || route.endpoint_state === 'access_protected' ? 'FAILED' : 'UNKNOWN'} label={route.hostname ? remoteRouteEndpointLabel(route.endpoint_state) : '—'} />{route.endpoint_detail && route.endpoint_state !== 'reachable' && <span className="secondary-text">{route.endpoint_detail}</span>}</td></tr>)}</tbody></table></div>}
	</div>
	return <section className="section-block first-section">
		<div className="section-heading"><div><h2>Remote access</h2><p>{!remote?.configured ? 'Choose how Fleet Manager and instance dashboards are published' : existingEndpoints ? 'Public URLs managed by your existing provider' : 'Pre-created Cloudflare tunnels connected by secure tokens'}</p></div><Status value={stateValue} label={stateLabel} /></div>
			{remote?.configured && !showEditor && configuration?.legacy_provider_managed && <div className="remote-access-boundary"><ShieldCheck size={17} /><div><strong>Legacy Cloudflare configuration</strong><span>This release keeps the existing configuration running. Disable it, then re-enable remote access with tunnel tokens to migrate.</span></div></div>}
			{remote?.configured && !showEditor && (existingEndpoints ? <div className="settings-grid">
			<div className="settings-row"><div><strong>Fleet Manager public URL</strong><span>Registered in Fleet; routing stays with your provider</span></div><div>{remote.admin.url ? <a href={remote.admin.url} target="_blank" rel="noreferrer"><strong>{remote.admin.url}</strong></a> : <strong>Not registered</strong>}<span>{remote.admin.url ? 'Opens through your existing provider' : 'Fleet Manager remains local-only'}</span></div></div>
			<div className="settings-row"><div><strong>Instance dashboard URLs</strong><span>Exact URLs for dashboards already published elsewhere</span></div><div><strong>{remote.instances.routes} registered</strong><span>Edit configuration to view or change each URL</span></div></div>
			<div className="settings-row"><div><strong>Ownership boundary</strong><span>Fleet stores URL mappings only</span></div><div><strong>External provider</strong><span>Fleet never creates, updates, verifies, or removes provider routes</span></div></div>
		</div> : <>
			<div className="remote-capability-grid" aria-label="Remote access capabilities">
				<div className="remote-capability-card">
					<div><strong>Fleet Manager access</strong><Status value={adminStatusValue} label={adminStatusLabel} /></div>
					{remote.admin.hostname ? <a href={remote.admin.url || `https://${remote.admin.hostname}`} target="_blank" rel="noreferrer">{remote.admin.hostname}</a> : <strong>Not configured</strong>}
					<span>The admin route remains manually managed in Cloudflare.</span>
				</div>
				<div className="remote-capability-card">
					<div><strong>Instance publishing</strong><Status value={configuration?.instance_publishing_configured ? 'READY' : 'INCOMPLETE'} label={configuration?.instance_publishing_configured ? 'Ready' : 'Incomplete'} /></div>
					<strong>{configuration?.instance_publishing_fleet_namespace ? `${configuration.instance_publishing_fleet_namespace}-*` : 'Namespace not configured'}</strong>
					<span>{remote.instances.routes} managed {plural(remote.instances.routes, 'instance')} in {configuration?.instance_publishing_zone || 'an unverified zone'}.</span>
				</div>
			</div>
			<details className="diagnostics-details remote-technical-details">
				<summary>Technical details</summary>
				<div className="settings-grid">
					<div className="settings-row"><div><strong>Admin public endpoint</strong><span>Actual response from the configured hostname</span></div><div><strong>{remoteRouteEndpointLabel(remote.admin.endpoint_state)}</strong><span>{remote.admin.endpoint_checked_at ? `Checked ${relativeTime(remote.admin.endpoint_checked_at)}` : 'Waiting for the connector'}</span>{remote.admin.endpoint_detail && <span className={adminEndpointFailed ? 'danger-text' : ''}>{remote.admin.endpoint_detail}</span>}</div></div>
					<div className="settings-row"><div><strong>Connector runtime</strong><span>Actual cloudflared process readiness</span></div><div><strong>Admin {remote.admin.connector_state || 'not checked'} · Instances {remote.instances.connector_state || 'not checked'}</strong><span>{remote.admin.connector_checked_at || remote.instances.connector_checked_at ? `Checked ${relativeTime(remote.admin.connector_checked_at || remote.instances.connector_checked_at || '')}` : 'Waiting for connector health checks'}</span>{(remote.admin.connector_error || remote.instances.connector_error) && <span className="danger-text">{remote.admin.connector_error || remote.instances.connector_error}</span>}</div></div>
					<div className="settings-row"><div><strong>Connector configuration</strong><span>Tokens are encrypted and written only to connector token files</span></div><div><strong>{remote.last_sync_at ? `Applied ${relativeTime(remote.last_sync_at)}` : 'Not applied'}</strong><span>Admin tunnel {remote.admin.tunnel_id || 'not reported'} · Instance tunnel {configuration?.instance_publishing_tunnel_id || 'not verified'}</span>{remote.last_error && <span className="danger-text">{remote.last_error}</span>}</div></div>
				</div>
				{routeInventory}
			</details>
		</>)}
		{showEditor && <div className="remote-access-editor">
			<div className="form-section-heading"><div><h3>Connection mode</h3><p>Choose who owns the public routing configuration.</p></div></div>
			<div className="remote-access-mode-picker">
				<label className={mode === 'managed_cloudflare' ? 'selected' : ''}><input type="radio" name="remote-access-mode" value="managed_cloudflare" checked={mode === 'managed_cloudflare'} disabled={remote?.configured} onChange={() => setMode('managed_cloudflare')} /><span><strong>Cloudflare tunnels</strong><small>Fleet runs one admin connector and one shared instance connector. Instance publishing includes route automation.</small></span></label>
				<label className={mode === 'existing_endpoints' ? 'selected' : ''}><input type="radio" name="remote-access-mode" value="existing_endpoints" checked={mode === 'existing_endpoints'} disabled={remote?.configured} onChange={() => setMode('existing_endpoints')} /><span><strong>Existing public endpoints</strong><small>Register URLs already served by Cloudflare, ngrok, Railway, or another provider. Fleet does not change or secure those routes.</small></span></label>
			</div>
			{remote?.configured && <div className="remote-access-boundary"><ShieldCheck size={17} /><div><strong>Mode is locked while active</strong><span>Disable remote access before changing ownership mode.</span></div></div>}
			{mode === '' ? <div className="remote-access-boundary"><ShieldCheck size={17} /><div><strong>No mode selected</strong><span>Choose existing Cloudflare tunnels or register endpoints that are already published by another provider.</span></div></div> : mode === 'managed_cloudflare' ? <>
				<div className="remote-access-boundary"><ShieldCheck size={17} /><div><strong>Tokens stay private</strong><span>Fleet encrypts tunnel and API tokens at rest and never returns them through the API.</span></div></div>
				{configuration?.legacy_provider_managed && <div className="remote-access-boundary"><ShieldCheck size={17} /><div><strong>Legacy configuration is active</strong><span>Disable remote access before migrating this configuration to tunnel tokens.</span></div></div>}
				<form className="remote-access-boundary-card" onSubmit={(event) => void saveAdminBoundary(event)}>
					<div className="remote-access-card-heading"><div><h3>Fleet Manager admin tunnel</h3><p>Publishes only the Fleet Manager admin surface.</p></div><Status value={adminStatusValue} label={adminStatusLabel} /></div>
					<div className="form-grid">
						{tunnelTokenField('admin', configuration?.admin_tunnel_token_configured)}
						{adminHostnameField()}
					</div>
					<div className="remote-origin-row"><div><strong>Cloudflare service URL</strong><span>Use this exact origin in the admin tunnel route. Fleet derives it from the Docker network.</span></div><code>{configuration?.admin_origin_service || 'http://control-plane:9180'}</code></div>
					{configuration?.admin_tunnel_token_configured && <div className="settings-grid"><div className="settings-row"><div><strong>Connector</strong><span>Actual cloudflared process readiness</span></div><div><Status value={adminConnectorReady ? 'READY' : remote?.admin.connector_state === 'unreachable' ? 'FAILED' : 'UNKNOWN'} label={adminConnectorReady ? 'Active' : remote?.admin.connector_state || 'Not checked'} />{remote?.admin.connector_error && <span className="danger-text">{remote.admin.connector_error}</span>}</div></div><div className="settings-row"><div><strong>Public endpoint</strong><span>Route remains manually managed in Cloudflare</span></div><div><Status value={adminEndpointReady ? 'READY' : adminEndpointFailed ? 'FAILED' : 'UNKNOWN'} label={remoteRouteEndpointLabel(remote?.admin.endpoint_state)} />{remote?.admin.endpoint_detail && <span className={adminEndpointFailed ? 'danger-text' : ''}>{remote.admin.endpoint_detail}</span>}</div></div></div>}
					<div className="remote-access-card-footer"><span>Saving this card does not change the instance tunnel.</span><button className="primary-button" disabled={syncing || Boolean(configuration?.legacy_provider_managed)}>{savingTarget === 'admin' ? <RefreshCw className="spin" size={16} /> : <ShieldCheck size={16} />}{savingTarget === 'admin' ? 'Saving admin tunnel' : 'Save admin tunnel'}</button></div>
				</form>
				<form className="remote-access-boundary-card" onSubmit={(event) => void saveInstancePublishing(event)}>
					<div className="remote-access-card-heading"><div><h3>Instance publishing</h3><p>One shared instance connector plus Cloudflare automation for Fleet-generated hostnames.</p></div><Status value={configuration?.instance_publishing_configured ? 'READY' : 'STOPPED'} label={configuration?.instance_publishing_configured ? 'Connected' : 'Not configured'} /></div>
					<div className="remote-access-boundary"><ShieldCheck size={17} /><div><strong>Fleet-owned resources only</strong><span>Fleet reconciles only DNS and ingress resources it recorded as owned, and preserves unrelated Cloudflare configuration.</span></div></div>
					<div className="form-grid remote-automation-grid">
						{tunnelTokenField('instances', configuration?.instances_tunnel_token_configured)}
						<label>Cloudflare Account ID<input required value={form.route_account_id} onChange={(event) => updateField('route_account_id', event.target.value)} /></label>
						<label>Zone ID<input required value={form.route_zone_id} onChange={(event) => updateField('route_zone_id', event.target.value)} /></label>
						{publishingAPITokenField()}
						<label>Fleet namespace<input required readOnly={namespaceLocked} placeholder="andes" pattern="[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?" maxLength={32} value={form.route_fleet_namespace} onChange={(event) => updateField('route_fleet_namespace', event.target.value.toLowerCase())} /><span>{namespaceLocked ? 'Locked while any instance dashboard is published.' : 'Lowercase letters, numbers, and hyphens. Used as the prefix for every generated hostname.'}</span></label>
					</div>
					<div className="remote-origin-row"><div><strong>Hostname format</strong><span>Generated by Fleet and read-only on each instance.</span></div><code>{form.route_fleet_namespace && (configuration?.instance_publishing_zone || 'example.com') ? `${form.route_fleet_namespace}-<instance>.${configuration?.instance_publishing_zone || 'example.com'}` : 'Available after namespace and zone are configured'}</code></div>
					<div className="remote-origin-row"><div><strong>Publishing zone</strong><span>Resolved and verified from the Cloudflare Zone ID.</span></div><code>{configuration?.instance_publishing_zone || 'Available after verification'}</code></div>
					<div className="remote-origin-row"><div><strong>Tunnel ID</strong><span>Extracted and validated from the shared instance tunnel token.</span></div><code>{configuration?.instance_publishing_tunnel_id || 'Available after verification'}</code></div>
					{routeInventory}
					<div className="remote-access-card-footer"><span>Connect verifies the token, API permissions, shared connector, and current Cloudflare configuration.</span><button className="primary-button" disabled={syncing || Boolean(configuration?.legacy_provider_managed)}>{savingTarget === 'publishing' ? <RefreshCw className="spin" size={16} /> : <ShieldCheck size={16} />}{savingTarget === 'publishing' ? 'Connecting and verifying' : 'Connect and verify'}</button></div>
				</form>
			</> : <>
				<form className="remote-access-boundary-card" onSubmit={(event) => void saveEndpoints(event)}>
					<div className="remote-access-card-heading"><div><h3>Public URLs</h3><p>Fleet stores these mappings only. Your provider remains authoritative.</p></div></div>
					<div className="form-grid external-endpoint-grid">
						<label>Fleet Manager URL<input type="url" placeholder="https://admin.example.com" value={adminURL} onChange={(event) => setAdminURL(event.target.value)} /><span>Optional when the admin surface remains local-only.</span></label>
						{instances.map((instance) => <label key={instance.id}>{instance.name} dashboard URL<input type="url" placeholder={`https://${instance.name}.example.com`} value={instanceURLs[instance.id] || ''} onChange={(event) => updateInstanceURL(instance.id, event.target.value)} /><span>Optional; leave blank when this dashboard is not published.</span></label>)}
					</div>
					<div className="remote-access-boundary"><ShieldCheck size={17} /><div><strong>Registration is not provider verification</strong><span>Fleet does not create routes, test authentication, or remove external provider resources in this mode.</span></div></div>
					<div className="remote-access-card-footer"><span>At least one public URL is required.</span><button className="primary-button" disabled={syncing}>{savingTarget === 'endpoints' ? <RefreshCw className="spin" size={16} /> : <ShieldCheck size={16} />}{savingTarget === 'endpoints' ? 'Saving endpoints' : 'Save endpoints'}</button></div>
				</form>
			</>}
			{remote?.configured && <div className="section-footer"><span>Disable removes both local connector configurations. Cloudflare routes, DNS, and Access policies remain unchanged.</span><div className="button-row">{editing && !managedIncomplete && <button type="button" className="secondary-button" onClick={() => setEditing(false)} disabled={syncing}>Cancel</button>}<button type="button" className="secondary-button danger-button remote-disable-button" onClick={() => void disable()} disabled={syncing}>{cleanupPending ? 'Retry cleanup' : 'Disable remote access'}</button></div></div>}
		</div>}
		{syncError && <div className="inline-error">{syncError}</div>}
		{remote?.configured && !showEditor && <div className="section-footer"><span>{existingEndpoints ? 'Fleet stores URL mappings; credentials and provider resources stay outside Fleet.' : cleanupPending && configuration?.legacy_provider_managed ? 'Legacy provider configuration is retained until Cloudflare route and DNS cleanup succeeds.' : cleanupPending ? 'Connectors are stopped. Fleet retains the boundary configuration until local connector cleanup succeeds.' : 'Tunnel tokens are encrypted by Fleet and never returned through the API.'}</span><div className="button-row">{!cleanupPending && !configuration?.legacy_provider_managed && <button className="secondary-button" onClick={() => setEditing(true)} disabled={syncing}>Edit configuration</button>}{!existingEndpoints && !cleanupPending && <button className="secondary-button" onClick={() => void reconcile()} disabled={loading || syncing || remote.state === 'syncing'}><RefreshCw size={16} className={syncing || remote.state === 'syncing' ? 'spin' : ''} />{syncing ? 'Checking' : 'Check connectors'}</button>}<button className="secondary-button danger-button remote-disable-button" onClick={() => void disable()} disabled={syncing}>{cleanupPending ? 'Retry cleanup' : 'Disable remote access'}</button></div></div>}
	</section>
}

function SystemGeneralPanel({ info, loading }: { info: SystemInfo | null; loading: boolean }) {
	const capacity = info?.capacity
	return <section className="section-block first-section">
		<div className="section-heading"><div><h2>System overview</h2><p>Essential control-plane state</p></div>{loading && <RefreshCw className="spin" size={17} />}</div>
		<div className="system-overview-band" aria-label="System summary">
			<div className="system-overview-card"><span>Fleet readiness</span>{info?.readiness ? <Status value={info.readiness.ready ? 'READY' : 'FAILED'} label={info.readiness.ready ? 'Ready' : 'Unavailable'} /> : <strong>{info ? 'Not reported' : 'Loading'}</strong>}<small>{info?.readiness?.last_checked ? `Checked ${relativeTime(info.readiness.last_checked)}` : info ? 'No readiness result from the API' : 'Waiting for readiness checks'}</small></div>
			<div className="system-overview-card"><span>Storage guardrail</span>{capacity ? <Status value={capacity.operations_safe ? 'READY' : 'FAILED'} label={capacity.operations_safe ? 'Operations safe' : 'Operations blocked'} /> : <strong>{info ? 'Not reported' : 'Loading'}</strong>}<small>{capacity ? `${formatBytes(capacity.free_bytes)} free · ${capacity.minimum_free_percent}% reserve` : info ? 'No capacity result from the API' : 'Waiting for capacity data'}</small></div>
			<div className="system-overview-card"><span>Fleet Manager</span><strong>{info ? `Version ${info.fleet_version}` : 'Loading'}</strong><small>Control plane and web interface</small></div>
		</div>
		<details className="diagnostics-details overview-technical-details">
			<summary>Technical details</summary>
			<div className="settings-grid">
				<div className="settings-row"><div><strong>Build</strong><span>Running Fleet Manager release identifier</span></div><code>{info?.build_id ?? 'Loading'}</code></div>
				<div className="settings-row"><div><strong>Host-local operator URL</strong><span>Reachable on the Fleet Manager host or through an SSH tunnel</span></div><code>{info?.operator_url ?? 'Loading'}</code></div>
				<div className="settings-row"><div><strong>State database</strong><span>Fleet configuration and operation records</span></div><code title={info?.database_path}>{info?.database_path ?? 'Loading'}</code></div>
			</div>
		</details>
	</section>
}

function BackupsPanel({ token, refreshSignal, info, systemLoading, reloadSystemInfo }: { token: string; refreshSignal: number; info: SystemInfo | null; systemLoading: boolean; reloadSystemInfo: () => Promise<void> }) {
  const [backups, setBackups] = useState<Backup[]>([])
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState('')
  const [backupError, setBackupError] = useState('')
	const [drillBusy, setDrillBusy] = useState(false)
	const [drillError, setDrillError] = useState('')
	const [kitBusy, setKitBusy] = useState(false)
	const [page, setPage] = useState(0)
	const pageSize = 8
	const loadController = useRef<AbortController | null>(null)
	const loadSequence = useRef(0)

  const loadBackups = useCallback(async () => {
    const sequence = loadSequence.current + 1
    loadSequence.current = sequence
    loadController.current?.abort()
    const controller = new AbortController()
    loadController.current = controller
    setLoading(true)
    try {
      const items = await apiRequest<Backup[]>(token, '/api/v1/backups', {
        cache: 'no-store',
        signal: controller.signal,
      })
      if (!controller.signal.aborted && sequence === loadSequence.current) {
        setBackups(items ?? [])
        setBackupError('')
      }
    } catch (requestError) {
      if (requestError instanceof DOMException && requestError.name === 'AbortError') return
      if (sequence === loadSequence.current) setBackupError(requestError instanceof Error ? requestError.message : 'Could not load backups')
    } finally {
      if (sequence === loadSequence.current) {
        if (loadController.current === controller) loadController.current = null
        setLoading(false)
      }
    }
  }, [token])

  useEffect(() => {
    const initial = window.setTimeout(() => void loadBackups(), 0)
    return () => {
      window.clearTimeout(initial)
      loadController.current?.abort()
    }
  }, [loadBackups, refreshSignal])

  const createBackup = async () => {
    setBusy('create')
    setBackupError('')
    try {
      await apiRequest<Backup>(token, '/api/v1/backups', { method: 'POST' })
      await loadBackups()
    } catch (requestError) {
      setBackupError(requestError instanceof Error ? requestError.message : 'Could not create backup')
    } finally {
      setBusy('')
    }
  }

  const verifyBackup = async (item: Backup) => {
    setBusy(`verify-${item.id}`)
    setBackupError('')
    try {
      await apiRequest<Backup>(token, `/api/v1/backups/${item.id}/verify`, { method: 'POST' })
      await loadBackups()
    } catch (requestError) {
      setBackupError(requestError instanceof Error ? requestError.message : 'Backup verification failed')
    } finally {
      setBusy('')
    }
  }

  const downloadBackup = async (item: Backup) => {
    setBusy(`download-${item.id}`)
    setBackupError('')
    try {
      await apiDownloadToFile(token, `/api/v1/backups/${item.id}/download`, item.filename)
      await loadBackups()
    } catch (requestError) {
      setBackupError(requestError instanceof Error ? requestError.message : 'Could not download backup')
    } finally {
      setBusy('')
    }
  }

  const deleteBackup = async (item: Backup) => {
    const confirmation = window.prompt(`Type ${item.filename} to permanently delete this backup.`)
    if (confirmation === null) return
    setBusy(`delete-${item.id}`)
    setBackupError('')
    try {
      await apiRequest<void>(token, `/api/v1/backups/${item.id}`, { method: 'DELETE', body: JSON.stringify({ confirm_filename: confirmation }) })
      await loadBackups()
    } catch (requestError) {
      setBackupError(requestError instanceof Error ? requestError.message : 'Could not delete backup')
    } finally {
      setBusy('')
    }
  }

  const copyBackupDigest = async (item: Backup) => {
    try {
      await navigator.clipboard.writeText(item.sha256)
    } catch (requestError) {
      setBackupError(requestError instanceof Error ? requestError.message : 'Could not copy backup digest')
    }
  }
	const drill = info?.recovery_drill
	const retention = info?.backup_retention ?? 0
	const retentionExceeded = retention > 0 && backups.length > retention
	const startRecoveryDrill = async () => {
		setDrillBusy(true)
		setDrillError('')
		try {
			await apiRequest(token, '/api/v1/system/recovery-drill', { method: 'POST' })
			await reloadSystemInfo()
		} catch (requestError) {
			setDrillError(requestError instanceof Error ? requestError.message : 'Recovery drill could not be started')
		} finally {
			setDrillBusy(false)
		}
	}
	const exportRecoveryKit = async () => {
		setKitBusy(true)
		setDrillError('')
		try {
			const timestamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\.\d{3}Z$/, 'Z')
			await apiDownloadToFile(token, '/api/v1/system/recovery-kit/download', `hermes-fleet-recovery-kit-${timestamp}.tar`, { method: 'POST' })
		} catch (requestError) {
			setDrillError(requestError instanceof Error ? requestError.message : 'Recovery kit could not be exported')
		} finally {
			setKitBusy(false)
		}
	}
	const drillSummary = drill?.status === 'PASSED'
		? `${drill.instance_backups_checked} instance backups and the control-plane backup verified`
		: drill?.status === 'INCOMPLETE'
			? drill.instances_without_backup === 1 ? '1 instance does not have a verified backup' : `${drill.instances_without_backup} instances do not have a verified backup`
			: drill?.status === 'FAILED'
				? drill.error || 'The last recovery drill failed'
				: drill?.status === 'RUNNING'
					? 'Verifying backups in isolated scratch storage'
					: 'No isolated recovery drill has been run'

	const pageCount = Math.max(1, Math.ceil(backups.length / pageSize))
	const safePage = Math.min(page, pageCount - 1)
	const visibleBackups = backups.slice(safePage * pageSize, (safePage + 1) * pageSize)

  return <section className="section-block first-section backup-section">
		<div className="section-heading"><div><h2>Backups &amp; recovery</h2><p>{retention ? `${backups.length}/${retention} control-plane backups retained` : `${backups.length} database ${plural(backups.length, 'backup')}`}</p></div><button className="primary-button" title={retentionExceeded ? 'Automatic backup pruning requires attention' : undefined} onClick={() => void createBackup()} disabled={busy !== '' || retentionExceeded}><Archive size={16} />{busy === 'create' ? 'Creating' : 'Create backup'}</button></div>
		<div className="backup-summary-band" aria-label="Backup and recovery summary">
			<div><span>Retention</span><strong>{retention ? `${backups.length}/${retention}` : 'Loading'}</strong><small>{retentionExceeded ? 'Pruning failed' : retention > 0 && backups.length === retention ? 'Rolling automatically' : 'Available'}</small></div>
			<div><span>Latest verified</span><strong>{backups[0]?.verified_at ? relativeTime(backups[0].verified_at) : 'None'}</strong><small>Control-plane database</small></div>
			<div><span>Recovery readiness</span>{drill ? <Status value={drill.status} label={drill.status === 'NEVER_RUN' ? 'Not tested' : drill.status.replace('_', ' ').toLowerCase()} /> : <strong>{info ? 'Not reported' : 'Loading'}</strong>}<small>{drill?.completed_at ? `Checked ${relativeTime(drill.completed_at)}` : info ? 'Run an isolated drill' : 'Waiting for recovery state'}</small></div>
		</div>
		{retentionExceeded && <div className="retention-warning" role="alert"><Archive size={18} /><div><strong>Automatic pruning failed</strong><span>Backup storage exceeded its rolling retention limit. Remove an older backup before creating another recovery point.</span></div></div>}
      <div className="backup-scope"><ShieldCheck size={18} /><div><strong>Fleet database only</strong><span>Includes Fleet configuration and operation records. Instance data, Host Agent enrollment, and matching <code>.env</code> encryption keys require their own backup path.</span></div></div>
		<div className="backup-recovery-panel">
			<div><strong>Recovery drill</strong><span>{drillSummary}{drill?.completed_at ? ` · ${relativeTime(drill.completed_at)}` : ''}</span></div>
			<div className="button-row"><button className="secondary-button" onClick={() => void startRecoveryDrill()} disabled={systemLoading || drillBusy || kitBusy || drill?.status === 'RUNNING'}>{drillBusy || drill?.status === 'RUNNING' ? <RefreshCw className="spin" size={15} /> : <ShieldCheck size={15} />}{drill?.status === 'RUNNING' ? 'Testing recovery' : 'Run recovery drill'}</button><button className="secondary-button" title={drill?.status === 'PASSED' ? 'Export the verified recovery kit' : 'Run and pass the recovery drill before exporting'} onClick={() => void exportRecoveryKit()} disabled={systemLoading || kitBusy || drill?.status !== 'PASSED'}>{kitBusy ? <RefreshCw className="spin" size={15} /> : <Download size={15} />}{kitBusy ? 'Preparing kit' : 'Export recovery kit'}</button></div>
		</div>
      {backupError && <div className="inline-error">{backupError}</div>}
		{drillError && <div className="inline-error">{drillError}</div>}
	  {loading ? <div className="empty-state"><RefreshCw className="spin" size={22} /><strong>Loading backups</strong></div> : backups.length === 0 ? <EmptyState icon={Archive} title="No backups yet" detail="Create a verified control-plane backup before upgrades or configuration changes." /> : <div className="table-wrap"><table className="provider-table backup-table"><thead><tr><th>Backup</th><th>Size</th><th>Integrity</th><th>Last verified</th><th><span className="sr-only">Actions</span></th></tr></thead><tbody>{visibleBackups.map((item) => <tr key={item.id}><td data-label="Backup"><strong>{item.filename}</strong><span className="secondary-text" title={item.sha256}>SHA-256 {item.sha256.slice(0, 16)}… · {relativeTime(item.created_at)}</span></td><td data-label="Size">{formatBytes(item.size_bytes)}</td><td data-label="Integrity"><Status value="VERIFIED" /></td><td data-label="Last verified">{relativeTime(item.verified_at)}</td><td data-label="Actions"><div className="row-actions"><button className="icon-button" title="Copy SHA-256" onClick={() => void copyBackupDigest(item)}><Copy size={15} /></button><button className="icon-button" title="Verify backup" onClick={() => void verifyBackup(item)} disabled={busy !== ''}><ShieldCheck size={15} /></button><button className="icon-button" title="Download backup" onClick={() => void downloadBackup(item)} disabled={busy !== ''}><Download size={15} /></button><button className="icon-button danger-button" title="Delete backup" onClick={() => void deleteBackup(item)} disabled={busy !== ''}><Trash2 size={15} /></button></div></td></tr>)}</tbody></table></div>}
		{backups.length > pageSize && <div className="pagination"><span>Page {safePage + 1} of {pageCount}</span><div><button className="secondary-button compact-button" onClick={() => setPage(Math.max(0, safePage - 1))} disabled={safePage === 0}>Previous</button><button className="secondary-button compact-button" onClick={() => setPage(Math.min(pageCount - 1, safePage + 1))} disabled={safePage >= pageCount - 1}>Next</button></div></div>}
    </section>
}

function CreateInstanceDialog({ hosts, token, onClose, onCreated, onOperation }: {
  hosts: Host[]
  token: string
  onClose: () => void
  onCreated: () => void
  onOperation: (operation: Operation) => void
}) {
  const onlineHosts = hosts.filter((host) => host.status === 'ONLINE')
  const initialHostID = onlineHosts[0]?.id ?? ''
	const [form, setForm] = useState({ name: '', host_id: initialHostID, hermes_version: '' })
	const [releaseCatalog, setReleaseCatalog] = useState<HermesReleaseCatalog | null>(null)
	const [releaseError, setReleaseError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')
	useEffect(() => {
		const controller = new AbortController()
		void apiRequest<HermesReleaseCatalog>(token, '/api/v1/hermes-releases', { signal: controller.signal }).then((catalog) => {
			if (controller.signal.aborted) return
			setReleaseCatalog(catalog)
			setReleaseError('')
			setForm((current) => ({ ...current, hermes_version: current.hermes_version || catalog.releases[0]?.version || '' }))
		}).catch((requestError) => {
			if (requestError instanceof DOMException && requestError.name === 'AbortError') return
			if (controller.signal.aborted) return
			setReleaseError(requestError instanceof Error ? requestError.message : 'Hermes versions could not be loaded')
		})
		return () => controller.abort()
	}, [token])
  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setSubmitting(true)
    setError('')
    try {
      const operation = await apiRequest<Operation>(token, '/api/v1/instances', { method: 'POST', body: JSON.stringify(form) })
      if (operation?.id && operation.created_at) onOperation(operation)
      onCreated()
    } catch (requestError) {
      setError(requestError instanceof Error ? requestError.message : 'Instance could not be created')
    } finally {
      setSubmitting(false)
    }
  }
	const close = () => { if (!submitting) onClose() }
	const { dialogRef, onKeyDown } = useDialogAccessibility(close, !submitting)
			return <div className="modal-backdrop" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) close() }}><div ref={dialogRef} className="modal" role="dialog" aria-modal="true" aria-labelledby="create-title" aria-busy={submitting} tabIndex={-1} onKeyDown={onKeyDown}><div className="modal-header"><div><h2 id="create-title">Create instance</h2><p>Choose a Hermes release; Fleet prepares its image when needed</p></div><button className="icon-button" onClick={close} title="Close" disabled={submitting}><X size={18} /></button></div><form onSubmit={submit}><div className="form-grid"><label>Instance name<input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value.toLowerCase() })} pattern="[a-z](?:[a-z0-9]|-){2,31}" placeholder="fleet-test-01" required disabled={submitting} /></label><label>Host<select value={form.host_id} onChange={(event) => setForm({ ...form, host_id: event.target.value })} required disabled={submitting}>{onlineHosts.map((host) => <option key={host.id} value={host.id}>{host.name}</option>)}</select></label><label>Hermes version<select value={form.hermes_version} onChange={(event) => setForm({ ...form, hermes_version: event.target.value })} required disabled={submitting || !releaseCatalog}>{!releaseCatalog && <option value="">Loading versions</option>}{releaseCatalog?.releases.map((release, index) => <option key={release.version} value={release.version}>{release.version}{index === 0 ? ' · latest' : ''}</option>)}</select></label></div>{releaseCatalog?.stale && <div className="inline-notice">GitHub release check is unavailable. Fleet is using the last verified catalog from {relativeTime(releaseCatalog.checked_at)}.</div>}{releaseError && <div className="inline-error">{releaseError}</div>}{error && <div className="inline-error">{error}</div>}<div className="modal-actions"><button type="button" className="secondary-button" onClick={close} disabled={submitting}>Cancel</button><button type="submit" className="primary-button" disabled={submitting || !form.host_id || !form.hermes_version}>{submitting ? 'Creating' : 'Create instance'}<Plus size={17} /></button></div></form></div></div>
}

function Status({ value, label = value }: { value: string; label?: string }) {
  return <span className={`status status-${value.toLowerCase().replace(/\s+/g, '-')}`}><span />{label}</span>
}

type InstanceStatusItem = {
	label: string
	value: string
	detail: string
}

function statusTone(value: string) {
	const normalized = value.toUpperCase()
	if (['READY', 'RUNNING', 'IN_SYNC', 'OK', 'ONLINE', 'REACHABLE'].includes(normalized)) return 'ready'
	if (['FAILED', 'MISSING', 'DRIFT', 'DEGRADED', 'UNAVAILABLE', 'CONFLICT'].includes(normalized)) return 'failed'
	if (['PENDING', 'PROVISIONING', 'UPDATING', 'RECONCILING', 'CHECKING', 'INCOMPLETE'].includes(normalized)) return 'progress'
	return 'neutral'
}

function InstanceStatusSummary({
	instanceName,
	items,
	summaryValue,
	summaryLabel,
}: {
	instanceName: string
	items: InstanceStatusItem[]
	summaryValue: string
	summaryLabel: string
}) {
	const [open, setOpen] = useState(false)
	const [position, setPosition] = useState({ top: 0, left: 0, ready: false })
	const popoverID = useId()
	const buttonRef = useRef<HTMLButtonElement | null>(null)
	const popoverRef = useRef<HTMLDivElement | null>(null)
	const closeTimerRef = useRef<number | null>(null)
	const cancelScheduledClose = () => {
		if (closeTimerRef.current === null) return
		window.clearTimeout(closeTimerRef.current)
		closeTimerRef.current = null
	}
	const show = () => {
		cancelScheduledClose()
		setOpen(true)
	}
	const scheduleClose = () => {
		cancelScheduledClose()
		closeTimerRef.current = window.setTimeout(() => setOpen(false), 120)
	}

	useLayoutEffect(() => {
		if (!open) return
		const updatePosition = () => {
			const button = buttonRef.current
			const popover = popoverRef.current
			if (!button || !popover) return
			const triggerRect = button.getBoundingClientRect()
			const margin = 12
			const gap = 7
			const popoverWidth = popover.offsetWidth
			const popoverHeight = popover.offsetHeight
			const left = Math.min(
				Math.max(margin, triggerRect.right - popoverWidth),
				Math.max(margin, window.innerWidth - popoverWidth - margin),
			)
			const roomBelow = window.innerHeight - triggerRect.bottom - gap - margin
			const top = roomBelow >= popoverHeight
				? triggerRect.bottom + gap
				: Math.max(margin, triggerRect.top - popoverHeight - gap)
			setPosition({ top, left, ready: true })
		}
		updatePosition()
		window.addEventListener('resize', updatePosition)
		window.addEventListener('scroll', updatePosition, true)
		return () => {
			window.removeEventListener('resize', updatePosition)
			window.removeEventListener('scroll', updatePosition, true)
		}
	}, [open])

	useEffect(() => {
		if (!open) return
		const closeOnOutsidePointer = (event: PointerEvent) => {
			const target = event.target as Node
			if (buttonRef.current?.contains(target) || popoverRef.current?.contains(target)) return
			setOpen(false)
		}
		document.addEventListener('pointerdown', closeOnOutsidePointer)
		return () => document.removeEventListener('pointerdown', closeOnOutsidePointer)
	}, [open])

	useEffect(() => () => {
		if (closeTimerRef.current !== null) window.clearTimeout(closeTimerRef.current)
	}, [])

	const popover = open ? createPortal(<div
		ref={popoverRef}
		id={popoverID}
		className="instance-status-popover"
		role="dialog"
		aria-label={`${instanceName} status details`}
		style={{ top: position.top, left: position.left, visibility: position.ready ? 'visible' : 'hidden' }}
		onMouseEnter={cancelScheduledClose}
		onMouseLeave={scheduleClose}
	>
		<strong>Instance status</strong>
		<ul>{items.map((item) => <li key={item.label}><span>{item.label}</span><Status value={item.value} label={item.detail} /></li>)}</ul>
	</div>, document.body) : null

	return <>
	<div
		className="instance-status-cluster"
		onMouseEnter={show}
		onMouseLeave={scheduleClose}
		onBlur={(event) => {
			if (!event.currentTarget.contains(event.relatedTarget)) setOpen(false)
		}}
		onKeyDown={(event) => {
			if (event.key !== 'Escape') return
			setOpen(false)
			buttonRef.current?.focus()
		}}
	>
		<button
			ref={buttonRef}
			type="button"
			className="instance-status-trigger"
			aria-expanded={open}
			aria-controls={popoverID}
			aria-haspopup="dialog"
			aria-label={`View status details: ${summaryLabel}`}
			onFocus={show}
			onClick={show}
		>
			<span className="instance-status-stack" aria-hidden="true">{items.map((item) => <span key={item.label} className={`instance-status-dot tone-${statusTone(item.value)}`} />)}</span>
			<Status value={summaryValue} label={summaryLabel} />
		</button>
	</div>
	{popover}
	</>
}

function CopyableDetailRow({ label, value }: { label: string; value: string }) {
	const [copied, setCopied] = useState(false)
	const copy = async () => {
		await navigator.clipboard.writeText(value)
		setCopied(true)
		window.setTimeout(() => setCopied(false), 1600)
	}
	return <div className="detail-row"><span>{label}</span><div className="copyable-detail-value"><code>{value}</code><button type="button" className="icon-button" onClick={() => void copy()} title={`Copy ${label}`} aria-label={`Copy ${label}`}><Copy size={15} /></button>{copied && <small>Copied</small>}</div></div>
}

function EmptyState({ icon: Icon, title, detail }: { icon: typeof Boxes; title: string; detail: string }) {
  return <div className="empty-state"><Icon size={24} /><strong>{title}</strong><span>{detail}</span></div>
}

function viewSubtitle(view: View) {
  if (view === 'hosts') return 'Machines running Hermes instances'
  if (view === 'chat') return 'Independent conversations across Hermes instances'
	if (view === 'outputs') return 'Hermes-generated files retained and governed by Fleet'
  if (view === 'alerts') return 'Actionable Fleet health and incident history'
  if (view === 'operations') return 'Lifecycle and audit history'
  if (view === 'system') return 'Fleet Manager configuration and maintenance'
  return 'Create and manage Hermes instances'
}

function systemSectionFromHash(): SystemSection {
	const section = window.location.hash.replace(/^#system\//, '')
	if (section === 'backups') return 'backups'
	if (section === 'remote-access') return 'remote-access'
	if (section === 'runtime-health') return 'runtime-health'
	return 'general'
}

function hermesUpdateStatusLabel(update: HermesUpdate | null, error: string) {
	if (error) return 'Update check failed'
	if (!update) return 'Checking for updates'
	const staleSuffix = update.official_stale ? ' · cached catalog' : ''
	if (update.official_status === 'UPDATE_AVAILABLE' && update.latest_release) return `Update available: ${update.latest_release.version}${staleSuffix}`
	if (update.official_status === 'CURRENT') return `Latest version installed${staleSuffix}`
	if (update.official_status === 'UNKNOWN') return 'Installed version could not be compared'
	return 'Update check failed'
}

function codexSignInDetail(connected: boolean, configured: boolean, active: boolean) {
	if (!connected) return configured ? 'Configuration saved separately' : 'Required before model setup'
	if (!configured) return 'Model not configured'
	return active ? 'Configured and ready' : 'Saved configuration needs applying'
}

function codexSetupIssueTitle(connected: boolean, configured: boolean, runtimeConfigurationDrift: boolean) {
	if (!connected && !configured) return 'Complete Codex setup'
	if (!connected) return 'Sign in to Codex'
	if (!configured) return 'Choose a Codex model'
	return runtimeConfigurationDrift ? 'Apply Codex configuration' : 'Review Codex setup'
}

function codexSetupDiagnostic(
	provider: string,
	authentication: ObservationCheck | undefined,
	runtimeConfiguration: ObservationCheck | undefined,
	connected: boolean,
	configured: boolean,
): ObservationCheck | null {
	if (provider !== 'openai-codex' || (!authentication && !runtimeConfiguration)) return null
	const checks = [authentication, runtimeConfiguration].filter((check): check is ObservationCheck => Boolean(check))
	if (checks.every((check) => check.status === 'OK')) return null
	const status = checks.some((check) => check.status === 'MISSING') ? 'MISSING' : checks.some((check) => check.status === 'DRIFT') ? 'DRIFT' : 'UNKNOWN'
	let detail = runtimeConfiguration?.detail ?? authentication?.detail ?? 'Codex setup needs attention'
	if (!connected && !configured) detail = 'Sign in to Codex, then choose the model and runtime settings for this instance.'
	else if (!connected) detail = 'The configuration is saved; sign in to Codex to make it usable.'
	else if (!configured) detail = 'Codex is signed in; choose the model, reasoning level, and service tier for this instance.'
	return { name: 'codex_setup', status, detail }
}

function buildFleetAlertRecords(hosts: Host[], instances: Instance[], operations: Operation[], health: RuntimeHealth | null, info: SystemInfo | null, backups: Backup[]) {
	const records: FleetAlertRecord[] = []
	const expectedAgentVersion = health?.compatibility.host_agent_version ?? info?.capabilities?.host_agent_version ?? ''
	for (const host of hosts) {
		if (host.status !== 'ONLINE') {
			records.push({ id: `host-offline:${host.id}`, state: 'ACTIVE', severity: 'CRITICAL', title: `${host.name} is offline`, detail: 'Fleet is no longer receiving the Host Agent heartbeat.', source: 'Host Agent', detectedAt: host.last_seen_at, evidence: [`Last heartbeat ${relativeTime(host.last_seen_at)}`, `${host.hostname} · ${host.os}/${host.arch}`, `Host ID ${host.id}`], action: { label: 'Open host', view: 'hosts' } })
		} else if (expectedAgentVersion && host.agent_version !== expectedAgentVersion) {
			records.push({ id: `host-agent:${host.id}`, state: 'ACTIVE', severity: 'WARNING', title: `${host.name} Host Agent is outdated`, detail: `Host Agent ${host.agent_version} does not match the Fleet compatibility contract.`, source: 'Compatibility', detectedAt: host.last_seen_at, evidence: [`Installed ${host.agent_version}`, `Expected ${expectedAgentVersion}`, `Host ID ${host.id}`], action: { label: 'Open host', view: 'hosts' } })
		}
	}
	for (const instance of instances) {
		const operationalStatus = instanceOperationalHealthStatus(instance)
		if (instance.status !== 'FAILED' && !['DEGRADED', 'MISSING'].includes(operationalStatus)) continue
		const critical = instance.status === 'FAILED' || operationalStatus === 'MISSING'
		records.push({ id: `instance:${instance.id}`, state: 'ACTIVE', severity: critical ? 'CRITICAL' : 'WARNING', title: `${instance.name} needs attention`, detail: instance.last_error || instance.observation?.summary || 'The managed runtime did not pass its latest authoritative check.', source: 'Instance health', detectedAt: instance.observation?.received_at || instance.updated_at, evidence: [`Runtime ${runtimeStatusLabel(instance.status)}`, `Health ${healthStatusLabel(operationalStatus)}`, `Host ${instance.host_name || instance.host_id}`], action: { label: 'Open instance diagnostics', view: 'fleet', instanceID: instance.id } })
	}
	for (const component of health?.components ?? []) {
		if (component.status !== 'degraded') continue
		if (component.component === 'remote_access' && ['pending', 'syncing'].includes(component.detail.toLowerCase())) continue
		records.push({ id: `component:${component.component}`, state: 'ACTIVE', severity: component.component === 'control_plane' ? 'CRITICAL' : 'WARNING', title: `${fleetComponentLabel(component.component)} is degraded`, detail: component.detail || 'The component is not passing its current health check.', source: 'Runtime health', detectedAt: component.updated_at, evidence: [component.detail, component.last_success_at ? `Last healthy ${relativeTime(component.last_success_at)}` : 'No successful check recorded'], action: { label: 'Open runtime health', view: 'system', systemSection: 'runtime-health' } })
	}
	if (info?.backup_retention && backups.length > info.backup_retention) {
		records.push({ id: 'backup-retention', state: 'ACTIVE', severity: 'WARNING', title: 'Backup rotation needs attention', detail: `Control-plane backups exceeded the rolling retention limit (${backups.length}/${info.backup_retention}). Automatic pruning did not complete.`, source: 'Control-plane backups', detectedAt: backups[0]?.created_at || info.readiness?.last_checked || new Date().toISOString(), evidence: [`${backups.length} retained backups`, `Retention limit ${info.backup_retention}`, 'Automatic pruning did not restore the configured limit'], action: { label: 'Open backups', view: 'system', systemSection: 'backups' } })
	}
	const recoveredByComponent = new Map<string, NonNullable<RuntimeHealth['recent_incidents']>>()
	for (const incident of (health?.recent_incidents ?? []).filter((item) => item.status === 'healthy')) {
		recoveredByComponent.set(incident.component, [...(recoveredByComponent.get(incident.component) ?? []), incident])
	}
	for (const incidents of [...recoveredByComponent.values()].slice(0, 5)) {
		const ordered = [...incidents].sort((left, right) => new Date(right.occurred_at).getTime() - new Date(left.occurred_at).getTime())
		const incident = ordered[0]
		const count = ordered.length
		records.push({ id: `recovered:${incident.component}:${incident.id}`, state: 'RECOVERED', resolution: 'RECOVERED', severity: 'WARNING', title: `${fleetComponentLabel(incident.component)} recovered`, detail: `${incident.detail || 'The component returned to a healthy state.'}${count > 1 ? ` · Occurred ${count} times` : ''}`, source: 'Runtime health', detectedAt: incident.occurred_at, evidence: [`Previous state ${incident.previous_status || 'degraded'}`, 'Current state healthy', ...(count > 1 ? [`${count} matching recoveries in recent history`] : [])], action: { label: 'Open runtime health', view: 'system', systemSection: 'runtime-health' } })
	}
	const operationGroups = groupOperations(operations)
	const successfulGroups = operationGroups.filter((group) => group.status === 'SUCCEEDED')
	const failedBuckets = new Map<string, OperationGroup[]>()
	for (const group of operationGroups.filter((item) => item.status === 'FAILED')) {
		const operation = latestOperation(group.operations)
		const failure = (operation.error || operation.progress?.detail || 'The recorded operation did not complete.').trim().toLowerCase().replace(/\s+/g, ' ')
		const key = `${operation.instance_id || 'fleet-manager'}:${group.type}:${failure}`
		failedBuckets.set(key, [...(failedBuckets.get(key) ?? []), group])
	}
	const failedHistory = [...failedBuckets.values()].map((groups) => [...groups].sort((left, right) => new Date(right.updatedAt).getTime() - new Date(left.updatedAt).getTime())).sort((left, right) => new Date(right[0].updatedAt).getTime() - new Date(left[0].updatedAt).getTime()).slice(0, 8)
	for (const groups of failedHistory) {
		const group = groups[0]
		const operation = latestOperation(group.operations)
		const scope = `${operation.instance_id || 'fleet-manager'}:${group.type}`
		const supersedingGroup = successfulGroups.filter((candidate) => {
			const candidateOperation = latestOperation(candidate.operations)
			return `${candidateOperation.instance_id || 'fleet-manager'}:${candidate.type}` === scope && new Date(candidate.updatedAt).getTime() > new Date(group.updatedAt).getTime()
		}).sort((left, right) => new Date(left.updatedAt).getTime() - new Date(right.updatedAt).getTime())[0]
		const occurrenceCount = groups.length
		const failureDetail = operation.error || operation.progress?.detail || 'The recorded operation did not complete.'
		const superseded = Boolean(supersedingGroup)
		records.push({
			id: `operation:${group.id}`,
			state: superseded ? 'RECOVERED' : 'FAILED',
			...(superseded ? { resolution: 'SUPERSEDED' as const } : {}),
			severity: 'WARNING',
			title: group.summary,
			detail: superseded ? `Previous failure was superseded by a later successful operation.${occurrenceCount > 1 ? ` ${occurrenceCount} matching failures were grouped.` : ''}` : `${failureDetail}${occurrenceCount > 1 ? ` · Occurred ${occurrenceCount} times` : ''}`,
			source: 'Operations',
			detectedAt: group.updatedAt,
			evidence: [`Type ${operationStageLabel(group.type)}`, `Actor ${operationActorLabel(group.actor)}`, `Last failure ${operation.id}`, ...(occurrenceCount > 1 ? [`${occurrenceCount} matching failed operations`] : []), ...(supersedingGroup ? [`Superseded by successful operation ${latestOperation(supersedingGroup.operations).id}`] : operation.progress?.action_code ? [`Action code ${operation.progress.action_code}`] : [])],
			action: { label: 'Open operations', view: 'operations' },
		})
	}
	return records.sort((left, right) => {
		if ((left.state === 'ACTIVE') !== (right.state === 'ACTIVE')) return left.state === 'ACTIVE' ? -1 : 1
		if (left.state === 'ACTIVE' && left.severity !== right.severity) return left.severity === 'CRITICAL' ? -1 : 1
		return new Date(right.detectedAt).getTime() - new Date(left.detectedAt).getTime()
	})
}

function fleetComponentLabel(component: string) {
	return ({ control_plane: 'Control plane', host_queue: 'Host work queue', remote_access: 'Remote access' } as Record<string, string>)[component] ?? sentenceCase(component)
}

function requestErrorMessage(value: unknown) {
	return value instanceof Error ? value.message : 'Source unavailable'
}

function isRepairableHermesProfileAccessError(value: unknown) {
	const message = requestErrorMessage(value)
	return /Hermes dashboard (?:session token is unavailable|returned HTTP (?:401|403)|profile login was rejected with HTTP (?:401|403))|Hermes profile access (?:is unavailable|failed)/i.test(message)
}

const codexSetupCheckNames = new Set(['codex_auth', 'runtime_configuration', 'codex_setup'])

function instanceOperationalHealthStatus(instance: Instance) {
	const observation = instance.observation
	if (!observation) return 'UNKNOWN'
	const failedChecks = (observation.checks ?? []).filter((check) => check.status !== 'OK')
	if (failedChecks.length > 0 && failedChecks.every((check) => codexSetupCheckNames.has(check.name))) {
		return 'IN_SYNC'
	}
	return observation.status
}

function instanceReadinessStatus(instance: Instance) {
	const operationalStatus = instanceOperationalHealthStatus(instance)
	if (!['IN_SYNC', 'OK'].includes(operationalStatus)) return operationalStatus
	const setupIncomplete = (instance.observation?.checks ?? []).some((check) =>
		codexSetupCheckNames.has(check.name) && check.status !== 'OK',
	)
	return setupIncomplete ? 'INCOMPLETE' : operationalStatus
}

function operationalHealthSummary(instance: Instance) {
	const status = instanceOperationalHealthStatus(instance)
	if (status === 'IN_SYNC') return 'Managed runtime is healthy'
	return instance.observation?.summary ?? 'Waiting for runtime inspection'
}

function installedHermesVersion(instance: Instance) {
	return instance.hermes_version ?? instance.observation?.hermes_version ?? 'Detecting'
}

function installedHermesVersionVerified(instance: Instance) {
	if (instance.hermes_version_verified !== undefined) return instance.hermes_version_verified
	return Boolean(instance.observation?.hermes_version)
}

function instanceHeaderSubtitle(instance: Instance) {
  return `${runtimeStatusLabel(instance.status)} · ${healthStatusLabel(instanceReadinessStatus(instance))}`
}

function runtimeStatusLabel(value: string) {
	return ({ RUNNING: 'Running', STOPPED: 'Stopped', PROVISIONING: 'Provisioning', RESTARTING: 'Restarting and verifying', UPDATING: 'Updating', RECONCILING: 'Fixing', BACKING_UP: 'Creating backup', RESTORING: 'Restoring backup', FAILED: 'Failed', DELETING: 'Deleting', DELETED: 'Deleted' } as Record<string, string>)[value] ?? sentenceCase(value)
}

function remoteRouteEndpointLabel(value?: RemoteAccessPublishedRoute['endpoint_state']) {
	return ({
		unchecked: 'Not checked',
		checking: 'Checking',
		propagating: 'DNS propagating',
		reachable: 'Reachable',
		access_protected: 'Protected by access policy',
		unavailable: 'Unavailable',
	} as const)[value ?? 'unchecked']
}

function remoteResourceStateLabel(value?: RemoteAccessPublishedRoute['dns_state'] | RemoteAccessPublishedRoute['route_state']) {
	return ({ pending: 'Pending', ready: 'Ready', conflict: 'Conflict', failed: 'Failed' } as const)[value ?? 'pending']
}

function publicationStageLabel(value: string) {
	return ({
		VALIDATING_HOSTNAME: 'Validate hostname',
		CREATING_DNS: 'Create or update DNS',
		UPDATING_INGRESS: 'Create or update tunnel ingress',
		VERIFYING_CLOUDFLARE: 'Verify Cloudflare configuration',
		CHECKING_PUBLIC_ENDPOINT: 'Check public endpoint',
		VALIDATING_TUNNEL_TOKEN: 'Validate tunnel token',
		VERIFYING_CLOUDFLARE_API: 'Verify Cloudflare API access',
		STARTING_CONNECTOR: 'Start connector',
		VERIFYING_CONNECTOR: 'Verify connector',
	} as Record<string, string>)[value] ?? sentenceCase(value)
}

function healthStatusLabel(value?: string) {
  return ({ IN_SYNC: 'Healthy', DEGRADED: 'Needs attention', MISSING: 'Unavailable', INCOMPLETE: 'Setup incomplete', UNKNOWN: 'Checking' } as Record<string, string>)[value ?? 'UNKNOWN'] ?? 'Checking'
}

function hostReadiness(host: Host, instances: Instance[], expectedAgentVersion: string, admissionOpen?: boolean) {
	if (host.status !== 'ONLINE') return 'OFFLINE'
	if (expectedAgentVersion && host.agent_version !== expectedAgentVersion) return 'AGENT_OUTDATED'
	if (instances.some((instance) => instance.status === 'RUNNING' && instanceOperationalHealthStatus(instance) === 'UNKNOWN')) return 'OBSERVER_STALE'
	if (instances.some((instance) => ['FAILED', 'DELETING'].includes(instance.status) || ['DEGRADED', 'MISSING'].includes(instanceOperationalHealthStatus(instance)))) return 'NEEDS_ATTENTION'
	if (admissionOpen === false) return 'CAPACITY_WARNING'
	return 'READY'
}

function hostReadinessLabel(value: string) {
	return ({ READY: 'Ready', NEEDS_ATTENTION: 'Needs attention', OBSERVER_STALE: 'Observations stale', CAPACITY_WARNING: 'Capacity warning', AGENT_OUTDATED: 'Agent outdated', OFFLINE: 'Offline' } as Record<string, string>)[value] ?? sentenceCase(value)
}

function operationStatusLabel(value: string) {
  return sentenceCase(value)
}

function operationActorLabel(value: string) {
	return ({ FLEET_ADMIN: 'Fleet admin', POLICY_CONTROLLER: 'Policy controller', SYSTEM: 'System' } as Record<string, string>)[value] ?? sentenceCase(value)
}

function operationDuration(start: string, end: string, status: string) {
	const startedAt = new Date(start).getTime()
	const endedAt = ['PENDING', 'RUNNING'].includes(status) ? Date.now() : new Date(end).getTime()
	const seconds = Math.max(0, Math.round((endedAt - startedAt) / 1000))
	if (seconds < 1) return '<1s'
	if (seconds < 60) return `${seconds}s`
	const minutes = Math.floor(seconds / 60)
	const remainder = seconds % 60
	return remainder === 0 ? `${minutes}m` : `${minutes}m ${remainder}s`
}

function operationTypeLabel(value: string) {
  const labels: Record<string, string> = {
    START: 'Start instance',
    STOP: 'Stop instance',
    DELETE: 'Delete instance',
    RETRY: 'Retry provisioning',
    PROVISION: 'Create instance',
    PROVIDER_BIND: 'Legacy configuration change',
    CODEX_AUTH: 'Authenticate Codex',
    CREDENTIAL_REVEAL: 'Reveal credentials',
		RECONCILE_IMAGE: 'Reconcile image',
		FIX_IMAGE_DRIFT: 'Fix image drift',
		REPAIR_RUNTIME: 'Repair and verify runtime',
		SYNC_RUNTIME: 'Complete Hermes setup',
		RESTORE: 'Restore backup',
		CONFIGURE_CODEX: 'Configure Codex',
		CONFIGURE_MESSAGING: 'Configure messaging',
		UPGRADE_HERMES: 'Update Hermes',
		HERMES_UPDATE_WORKFLOW: 'Hermes update',
		HERMES_RUNTIME_REFRESH_WORKFLOW: 'Managed runtime maintenance',
		ROLLOUT_POLICY: 'Roll out policy',
		REFRESH_DIAGNOSTICS_CAMPAIGN: 'Refresh diagnostics campaign',
  }
  return labels[value] ?? sentenceCase(value)
}

function sentenceCase(value: string) {
  const normalized = value.replace(/[_-]+/g, ' ').toLowerCase()
  return normalized.charAt(0).toUpperCase() + normalized.slice(1)
}

function plural(count: number, singular: string, pluralForm = `${singular}s`) {
  return count === 1 ? singular : pluralForm
}

function relativeTime(timestamp: string) {
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(timestamp).getTime()) / 1000))
  if (seconds < 60) return `${seconds}s ago`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
  return `${Math.floor(seconds / 86400)}d ago`
}

function chatTimestamp(timestamp: string, now = Date.now()) {
	const value = new Date(timestamp)
	if (Number.isNaN(value.getTime())) return timestamp
	if (Math.max(0, now - value.getTime()) < 60000) return 'Just now'
	const current = new Date(now)
	const sameDay = value.getFullYear() === current.getFullYear()
		&& value.getMonth() === current.getMonth()
		&& value.getDate() === current.getDate()
	if (sameDay) {
		return new Intl.DateTimeFormat('en-GB', { hour: '2-digit', minute: '2-digit' }).format(value)
	}
	return new Intl.DateTimeFormat('en-GB', {
		day: '2-digit',
		month: 'short',
		...(value.getFullYear() === current.getFullYear() ? {} : { year: 'numeric' as const }),
		hour: '2-digit',
		minute: '2-digit',
	}).format(value)
}

function chatRailTimestamp(timestamp: string, now = Date.now()) {
	const value = new Date(timestamp)
	if (Number.isNaN(value.getTime())) return { date: '', time: timestamp, label: timestamp }
	if (Math.max(0, now - value.getTime()) < 60000) return { date: '', time: 'Just now', label: 'Just now' }
	const current = new Date(now)
	const sameDay = value.getFullYear() === current.getFullYear()
		&& value.getMonth() === current.getMonth()
		&& value.getDate() === current.getDate()
	const time = new Intl.DateTimeFormat('en-GB', { hour: '2-digit', minute: '2-digit' }).format(value)
	if (sameDay) return { date: '', time, label: time }
	const date = new Intl.DateTimeFormat('en-GB', {
		day: '2-digit',
		month: 'short',
		...(value.getFullYear() === current.getFullYear() ? {} : { year: '2-digit' as const }),
	}).format(value)
	return { date, time, label: `${date}, ${time}` }
}

function chatIdentityCode(session: Pick<ChatSession, 'id' | 'title'>) {
	const titleCode = session.title.match(/\b(\d{3})\s*$/)?.[1]
	if (titleCode) return titleCode
	let hash = 0
	for (const character of session.id) {
		hash = (Math.imul(hash, 31) + character.charCodeAt(0)) >>> 0
	}
	return String(100 + (hash % 900))
}

function normalizeChatSessionTitle(session: ChatSession): ChatSession {
	if (!/^New Chat \d{3}$/.test(session.title)) return session
	return { ...session, title: `Chat ${chatIdentityCode(session)}` }
}

function fullChatTimestamp(timestamp: string) {
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

function observationTimestamp(timestamp?: string) {
  return validObservationTimestamp(timestamp) ? `Received ${relativeTime(timestamp as string)}` : 'Awaiting first report'
}

function validObservationTimestamp(timestamp?: string) {
  if (!timestamp) return false
  const date = new Date(timestamp)
  return !Number.isNaN(date.getTime()) && date.getUTCFullYear() >= 2000
}

function observationCheckLabel(name: string) {
	if (name === 'codex_setup') return 'Codex setup'
  if (name === 'codex_auth') return 'Codex authentication'
  if (name === 'runtime_configuration') return 'Hermes setup'
  return name.split('_').map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join(' ')
}

function runtimeRecoveryTitle(status: NonNullable<Instance['runtime_remediation']>['status']) {
  if (status === 'MONITORING') return 'Confirming runtime drift'
  if (status === 'READY') return 'Preparing automatic recovery'
  if (status === 'QUEUED') return 'Automatic recovery queued'
  if (status === 'VERIFYING') return 'Verifying automatic recovery'
  if (status === 'WAITING') return 'Automatic recovery will retry'
  if (status === 'COOLDOWN') return 'Automatic recovery is cooling down'
  if (status === 'EXHAUSTED') return 'Automatic recovery stopped after 9 attempts'
  return 'Automatic recovery was stopped'
}

function runtimeRecoveryStatusLabel(status: NonNullable<Instance['runtime_remediation']>['status']) {
  if (status === 'MONITORING') return 'Monitoring'
  if (status === 'READY') return 'Preparing'
  if (status === 'QUEUED') return 'Queued'
  if (status === 'VERIFYING') return 'Verifying'
  if (status === 'WAITING') return 'Waiting'
  if (status === 'COOLDOWN') return 'Cooldown'
  if (status === 'EXHAUSTED') return 'Limit reached'
  return 'Stopped'
}

function runtimeRecoveryDetail(remediation: NonNullable<Instance['runtime_remediation']>) {
  if (remediation.status === 'MONITORING') {
    return 'Fleet waits for a second fresh drift report before repairing managed services.'
  }
  if (remediation.status === 'READY') {
    return 'Fleet confirmed the issue and is preparing the next managed repair attempt.'
  }
  if (remediation.status === 'QUEUED') {
    return `Attempt ${remediation.total_attempts} is queued on the Host Agent.`
  }
  if (remediation.status === 'VERIFYING') {
    return `Attempt ${remediation.total_attempts} finished. Fleet is waiting for a fresh health report.`
  }
  if (remediation.status === 'WAITING' || remediation.status === 'COOLDOWN') {
    return remediation.next_attempt_at
      ? `Next attempt ${relativeTimeFuture(remediation.next_attempt_at)} if runtime drift remains.`
      : 'Fleet is waiting for the next fresh health report.'
  }
  if (remediation.status === 'EXHAUSTED') {
    return 'No more automatic repairs will run. Review diagnostics before choosing a manual repair.'
  }
  return 'No more automatic repairs will run for this unchanged runtime issue.'
}

function runtimeRecoveryPhaseLabel(phase: number) {
  if (phase === 1) return 'restart managed services'
  if (phase === 2) return 'recreate managed containers'
  return 'rebuild managed services; data volume preserved'
}

function relativeTimeFuture(timestamp: string) {
  const seconds = Math.max(0, Math.floor((new Date(timestamp).getTime() - Date.now()) / 1000))
  if (seconds < 60) return `in ${seconds}s`
  return `in ${Math.ceil(seconds / 60)}m`
}

function formatBytes(bytes: number) {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`
  if (bytes < 1024 * 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} GiB`
  return `${(bytes / (1024 * 1024 * 1024 * 1024)).toFixed(1)} TiB`
}

function sleep(milliseconds: number, signal?: AbortSignal) {
  return new Promise<void>((resolve, reject) => {
    const finish = () => {
      signal?.removeEventListener('abort', abort)
      resolve()
    }
    const timer = window.setTimeout(finish, milliseconds)
    if (!signal) return
    const abort = () => {
      window.clearTimeout(timer)
      signal.removeEventListener('abort', abort)
      reject(new DOMException('Aborted', 'AbortError'))
    }
    if (signal.aborted) abort()
    else signal.addEventListener('abort', abort, { once: true })
  })
}
