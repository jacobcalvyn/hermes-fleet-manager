import { expect, test, type Page } from '@playwright/test'

const now = '2026-07-27T00:00:00Z'
const releaseSource = 'NousResearch/hermes-agent GitHub Releases'
const adminTunnelID = '11111111-1111-4111-8111-111111111111'
const instancesTunnelID = '22222222-2222-4222-8222-222222222222'
const host = {
  id: 'host-regression',
  name: 'local-mac',
  hostname: 'local-mac',
  os: 'darwin',
  arch: 'arm64',
  agent_version: '0.10.0',
  status: 'ONLINE',
  last_seen_at: now,
  created_at: now,
}
const releases = [
  {
    version: '0.19.0',
    tag: 'v2026.7.20',
    commit: '3ef6bbd201263d354fd83ec55b3c306ded2eb72a',
    image: 'local/hermes-fleet-runtime:0.19.0-3ef6bbd20126',
    url: 'https://github.com/NousResearch/hermes-agent/releases/tag/v2026.7.20',
    published_at: now,
  },
]

type BaseRouteOptions = {
  operations?: unknown[]
  olderOperations?: unknown[]
  onOperationsRequest?: () => void
  onOlderOperationsRequest?: () => void
  onObservationRefresh?: () => void
}

async function installBaseRoutes(page: Page, options: BaseRouteOptions = {}) {
  await page.addInitScript(() => sessionStorage.setItem('fleet-admin-token', 'e2e-token'))
  await page.route(/\/api\/v1\/operations(?:\?.*)?$/, async (route) => {
    const cursor = new URL(route.request().url()).searchParams.get('cursor')
    if (cursor) {
      options.onOlderOperationsRequest?.()
      await route.fulfill({ json: { items: options.olderOperations ?? [] } })
      return
    }
    options.onOperationsRequest?.()
    await route.fulfill({
      json: {
        items: options.operations ?? [],
        ...(options.olderOperations ? { next_cursor: 'opaque-page-2' } : {}),
      },
    })
  })
  await page.route('**/api/v1/hermes-releases', async (route) => {
    await route.fulfill({ json: { source: releaseSource, checked_at: now, releases } })
  })
  await page.route('**/api/v1/events', async (route) => {
    await route.fulfill({ status: 200, contentType: 'text/event-stream', body: '' })
  })
  await page.route('**/api/v1/instances/*/hermes-update', async (route) => {
    await route.fulfill({
      json: {
        current_version: '0.19.0',
        current_image: releases[0].image,
        target_version: '0.19.0',
        target_source: releases[0].commit,
        target_image: releases[0].image,
        official_status: 'CURRENT',
        update_kind: 'NONE',
        official_source: releaseSource,
        official_checked_at: now,
        latest_release: releases[0],
        available: false,
        eligible: false,
        reason: 'No newer installable Hermes version is available',
      },
    })
  })
  await page.route('**/api/v1/instances/*/observations/refresh', async (route) => {
    options.onObservationRefresh?.()
    await route.fulfill({ status: 202, json: {} })
  })
}

async function openFleet(
  page: Page,
  overview: Record<string, unknown>,
  options: BaseRouteOptions = {},
) {
  await installBaseRoutes(page, options)
  await page.route('**/api/v1/overview', async (route) => {
    await route.fulfill({ json: overview })
  })
  await page.goto('/')
  await navigateToInstances(page)
}

async function navigateToInstances(page: Page) {
  const instancesButton = page.getByRole('button', { name: 'Instances', exact: true })
  if (!await instancesButton.isVisible()) {
    await page.getByTitle('Open navigation').click()
  }
  await instancesButton.click()
  await expect(page.getByRole('heading', { name: 'Instances', level: 1 })).toBeVisible()
}

function operation(index: number, overrides: Record<string, unknown> = {}) {
  const timestamp = new Date(Date.parse(now) - index * 60_000).toISOString()
  return {
    id: `operation-${String(index).padStart(3, '0')}`,
    instance_id: 'instance-regression',
    actor: 'FLEET_ADMIN',
    type: 'START',
    status: 'SUCCEEDED',
    summary: `Operation ${index}`,
    created_at: timestamp,
    updated_at: timestamp,
    ...overrides,
  }
}

function codexInstance(recommendedModel?: string, modelCatalog = ['model-alpha', 'model-beta']) {
  return {
    id: 'instance-codex-sync',
    name: 'fleet-codex-sync',
    host_id: host.id,
    host_name: host.name,
    status: 'RUNNING',
    image: releases[0].image,
    image_id: `sha256:${'a'.repeat(64)}`,
    provider: 'openai-codex',
    model: '',
    reasoning: '',
    service_tier: '',
    codex_configured: false,
    api_port: 8650,
    dashboard_port: 9130,
    project_name: 'hermes-fleet-codex-sync',
    data_volume: 'hermes-fleet-codex-sync-data',
    managed_path: '/tmp/hermes-fleet-codex-sync',
    created_at: now,
    updated_at: now,
    observation: {
      instance_id: 'instance-codex-sync',
      hermes_version: '0.19.0',
      model_catalog: modelCatalog,
      recommended_model: recommendedModel,
      status: 'DEGRADED',
      summary: 'Codex configuration required',
      received_at: now,
      checks: [
        { name: 'codex_auth', status: 'OK', detail: 'Codex authentication is connected' },
        { name: 'runtime_configuration', status: 'DRIFT', detail: 'Codex configuration has not been saved' },
      ],
    },
  }
}

test('Chat renders an optimistic message before queue admission and keeps transport details out of the UI', async ({ page }) => {
  const qaScreenshotPath = process.env.FLEET_CHAT_QA_SCREENSHOT
  const consoleErrors: string[] = []
  const failedResponses: string[] = []
  page.on('console', (message) => {
    if (message.type() === 'error') consoleErrors.push(message.text())
  })
  page.on('response', (response) => {
    if (response.status() >= 400) failedResponses.push(`${response.status()} ${response.url()}`)
  })
  if (qaScreenshotPath) await page.setViewportSize({ width: 1207, height: 1044 })

  const instance = {
    ...codexInstance('model-alpha'),
    model: 'model-alpha',
    reasoning: 'medium',
    service_tier: 'normal',
    codex_configured: true,
  }
  let session = {
    id: 'chat-session-optimistic',
    instance_id: instance.id,
    instance_name: instance.name,
    title: 'Chat with fleet-codex-sync',
	model: 'model-alpha',
	reasoning: 'medium',
	service_tier: 'normal',
    status: 'ACTIVE',
    message_count: 0,
	last_message_preview: 'Current session',
	response_in_progress: false,
	last_event_id: 0,
    created_at: now,
	updated_at: now,
  }
	const longToolCode = [
		'from hermes_tools import write_file',
		'import urllib.request,csv,io,json,re,time',
		"rows=list(csv.reader(io.StringIO(urllib.request.urlopen(url,timeout=30).read().decode('utf-8-sig'))))",
		"write_file('/root/dashboard_redirect.html', html)",
		"print(json.dumps({'status':'complete','records':len(rows)}))",
	].join('\n')
	const longSkillArguments = { name: 'operations-dashboard-auditing', code: longToolCode }
	const longSkillArgumentsText = JSON.stringify(longSkillArguments)
	const backgroundTime = new Date(new Date(now).getTime() - 60_000).toISOString()
	const backgroundCompletedTime = new Date(new Date(now).getTime() + 1_000).toISOString()
	const backgroundSession = {
		...session,
		id: 'chat-session-background',
		title: 'Chat with background',
		message_count: 1,
		last_message_id: 'background-user',
		last_message_role: 'user',
		last_message_preview: 'Inspect background runtime',
		last_message_at: backgroundTime,
		response_in_progress: true,
		last_event_id: 2,
		created_at: backgroundTime,
		updated_at: backgroundTime,
	}
  let queued = false
  let deleted = false
	let sessionPatch: Record<string, string> | undefined
  await page.route(/\/api\/v1\/chats$/, async (route) => route.fulfill({ json: deleted ? [] : [
		session,
		queued ? {
			...backgroundSession,
			message_count: 2,
			last_message_id: 'background-assistant',
			last_message_role: 'assistant',
			last_message_preview: 'Background response is ready',
			last_message_at: backgroundCompletedTime,
			response_in_progress: false,
			last_event_id: 4,
			updated_at: backgroundCompletedTime,
		} : backgroundSession,
	] }))
  await page.route(/\/api\/v1\/chats\/chat-session-optimistic$/, async (route) => {
    if (route.request().method() === 'DELETE') {
      deleted = true
      await route.fulfill({ status: 204 })
      return
    }
	if (route.request().method() === 'PATCH') {
		sessionPatch = route.request().postDataJSON() as Record<string, string>
		session = { ...session, ...sessionPatch }
		await route.fulfill({ json: session })
		return
	}
    await route.fulfill({ json: {
    protocol_version: 2,
    session: { ...session, message_count: queued ? 2 : 1 },
    messages: [{
      id: 'chat-message-markdown',
      session_id: session.id,
	  operation_id: 'chat-operation-completed',
      role: 'assistant',
      content: '**Fleet ready**\n\n- DNS verified\n- Tunnel connected\n\n[Open dashboard](https://example.com)\n\n`fleet status`\n\n```json\n{"status":"ready"}\n```\n\n<img src=x onerror=alert(1)>',
      status: 'SUCCEEDED',
      created_at: now,
      updated_at: now,
    }, ...(queued ? [{
      id: 'chat-message-persisted',
      session_id: session.id,
      operation_id: 'chat-operation-optimistic',
      role: 'user',
      content: 'inspect the current runtime',
      status: 'PENDING',
      created_at: now,
      updated_at: now,
    }] : [])],
	events: [{
	  id: 100,
	  sequence: 1,
	  session_id: session.id,
	  operation_id: 'chat-operation-completed',
	  type: 'ASSISTANT_ACTIVITY',
	  payload: {
		kind: 'activity',
		event: 'tool.completed',
		data: JSON.stringify({ type: 'tool.completed', tool_name: 'session_status', call_id: 'call-completed', status: 'completed' }),
	  },
	  created_at: now,
	}, ...(queued ? [
	  { id: 1, sequence: 1, data: { type: 'tool.progress', tool_name: '_thinking', delta: 'Inspecting the current Hermes session' } },
	  { id: 2, sequence: 2, data: { type: 'tool.started', tool_name: 'skill_view', call_id: 'call-skill', preview: 'operations-dashboard-auditing', args: longSkillArguments, status: 'running' } },
	  { id: 3, sequence: 3, data: { type: 'tool.completed', tool_name: 'skill_view', call_id: 'call-skill', args: longSkillArguments, status: 'completed', duration: 0.1 } },
	  { id: 4, sequence: 4, data: { type: 'tool.delta', tool: 'browser_type', call_id: 'call-orphan', arguments: { url: 'https://example.test', value: 'upt posind3m4s e1' } } },
	  { id: 5, sequence: 5, data: { type: 'tool.started', tool_name: 'browser_navigate', call_id: 'call-navigate', args: { url: 'https://example.test' }, status: 'running' } },
	  { id: 6, sequence: 6, data: { type: 'tool.completed', tool_name: 'browser_navigate', call_id: 'call-navigate', args: { url: 'https://example.test' }, status: 'completed', duration_ms: 20700 } },
	].map((event) => ({
	  ...event,
	  session_id: session.id,
	  operation_id: 'chat-operation-optimistic',
	  type: 'ASSISTANT_ACTIVITY' as const,
	  payload: {
		kind: 'activity' as const,
		event: event.data.type,
		data: JSON.stringify(event.data),
	  },
	  created_at: now,
	})) : [])],
	...(queued ? {
		active_response: { operation_id: 'chat-operation-optimistic', state: 'RUN_QUEUED', last_sequence: 6 },
	  } : {}),
	  last_cursor: 100,
    } })
  })
  await page.route(/\/api\/v1\/chats\/chat-session-optimistic\/events(?:\?.*)?$/, async (route) => {
    await route.fulfill({ status: 200, contentType: 'text/event-stream', body: ': keepalive\n\n' })
  })
  await page.route(/\/api\/v1\/chats\/chat-session-optimistic\/messages$/, async (route) => {
    await new Promise((resolve) => setTimeout(resolve, 500))
    queued = true
    await route.fulfill({ status: 202, json: {
      id: 'chat-operation-optimistic',
      instance_id: instance.id,
      actor: 'FLEET_ADMIN',
      type: 'CHAT_MESSAGE',
      status: 'PENDING',
      summary: 'Send chat message',
      created_at: now,
      updated_at: now,
    } })
  })
	await page.route(/\/api\/v1\/artifacts(?:\?.*)?$/, async (route) => {
		await route.fulfill({ json: { items: [{
			id: 'artifact-output-regression',
			instance_id: instance.id,
			instance_name: instance.name,
			session_id: session.id,
			session_title: session.title,
			operation_id: 'chat-operation-completed',
			name: 'fleet-report.txt',
			kind: 'file',
			media_type: 'text/plain',
			size_bytes: 12,
			status: 'ready',
			created_at: now,
			expires_at: '2026-08-27T00:00:00Z',
			download_url: '/api/v1/artifacts/artifact-output-regression/download',
		}] } })
	})
	await page.route('**/api/v1/artifacts/usage', async (route) => {
		await route.fulfill({ json: {
			total_bytes: 12,
			total_max_bytes: 2147483648,
			session_max_bytes: 104857600,
			instance_max_bytes: 536870912,
			retention_hours: 720,
			status_counts: { ready: 1 },
			instances: { [instance.id]: 12 },
			sessions: { [session.id]: 12 },
		} })
	})
  await page.route('**/api/v1/system/remote-access/configuration', async (route) => {
    await route.fulfill({ json: {
      mode: 'local_only',
      admin_hostname: '',
      admin_tunnel_token_configured: false,
      instances_tunnel_token_configured: false,
      legacy_provider_managed: false,
      admin_url: '',
      instance_endpoints: [],
      admin_origin_service: '',
      instance_routes: [],
    } })
  })

  await openFleet(page, { hosts: [host], instances: [instance], operations: [] })
  await page.getByRole('button', { name: 'Chat', exact: true }).click()
  await expect(page.getByRole('heading', { name: session.title })).toBeVisible()
	const metadata = page.locator('.chat-thread-metadata')
	await expect(metadata.locator('dt').first()).toHaveText('ID')
	await expect(metadata.locator('dt[title="Instance"] img')).toHaveAttribute('src', '/hermes-logo.png')
	await expect(metadata.locator('dt[title="Model"] svg')).toBeVisible()
	await expect(metadata.locator('dt[title="Reasoning"] svg')).toBeVisible()
	await expect(metadata.locator('dt[title="Service tier"] svg')).toBeVisible()
	await expect(metadata.locator('dd')).toHaveText([session.id, instance.name, 'model-alpha', 'medium', 'normal'])
	await page.getByRole('button', { name: 'Show chats sidebar' }).click()
	await page.getByRole('button', { name: `Open outputs for ${instance.name}` }).click()
	await expect(page.getByRole('heading', { name: 'Outputs', level: 1 })).toBeVisible()
	await expect(page.getByLabel('Filter outputs by instance')).toHaveValue(instance.id)
	await page.getByRole('button', { name: 'Open chat' }).click()
	await expect(page.getByRole('heading', { name: session.title })).toBeVisible()
	await expect(page.getByRole('button', { name: 'Hide chats sidebar' })).toBeVisible()
	await page.getByRole('button', { name: 'Edit session configuration' }).click()
	await page.getByLabel('Session model').selectOption('model-beta')
	await page.getByLabel('Session reasoning').selectOption('high')
	await page.getByLabel('Session service tier').selectOption('priority')
	await page.getByRole('button', { name: 'Save session configuration' }).click()
	await expect.poll(() => sessionPatch).toEqual({ model: 'model-beta', reasoning: 'high', service_tier: 'priority' })
	await expect(page.locator('.chat-thread-metadata dd')).toHaveText([session.id, instance.name, 'model-beta', 'high', 'priority'])
  await expect(page.getByText('Target:', { exact: false })).toHaveCount(0)
  await expect(page.getByText('Session context', { exact: false })).toHaveCount(0)
  const markdownBubble = page.locator('.chat-bubble.chat-assistant').filter({ hasText: 'Fleet ready' })
  await expect(markdownBubble.locator('strong')).toHaveText('Fleet ready')
  await expect(markdownBubble.locator('li')).toHaveCount(2)
  await expect(markdownBubble.getByRole('link', { name: 'Open dashboard' })).toHaveAttribute('target', '_blank')
  await expect(markdownBubble.getByRole('link', { name: 'Open dashboard' })).toHaveAttribute('rel', 'noopener noreferrer')
  await expect(markdownBubble.locator('.chat-code-block code')).toContainText('{"status":"ready"}')
  await expect(markdownBubble.getByRole('button', { name: 'Copy code' })).toBeVisible()
	await expect(markdownBubble.locator('.chat-assistant-response')).toBeVisible()
	await expect(markdownBubble.locator('.chat-response-hermes-icon img')).toHaveAttribute('src', '/hermes-logo.png')
	const completedProcess = markdownBubble.locator('.chat-process-card-completed')
	const completedProcessSummary = completedProcess.getByRole('button', { name: '1 activity' })
	await expect(completedProcessSummary).toHaveText('1 activity')
	await expect(completedProcessSummary).not.toContainText('Timeline')
	await expect(completedProcess.locator('.chat-process-fleet-icon')).toBeVisible()
	const completedSummaryTop = await completedProcessSummary.evaluate((element) => element.getBoundingClientRect().top)
	await completedProcessSummary.click()
	await page.waitForTimeout(450)
	const expandedSummaryTop = await completedProcessSummary.evaluate((element) => element.getBoundingClientRect().top)
	expect(Math.abs(expandedSummaryTop - completedSummaryTop)).toBeLessThanOrEqual(2)
	await expect(page.getByRole('button', { name: 'Hide chats sidebar' })).toBeVisible()
	await expect(page.locator('.chat-session-list time').first()).toBeVisible()
	const expandedBackgroundItem = page.locator('.chat-session-item').filter({ hasText: 'Chat with background' })
	await expect(expandedBackgroundItem.locator('.chat-session-select')).toBeVisible()
	await expect(expandedBackgroundItem.locator('.spin')).toBeVisible()

  await page.getByRole('textbox', { name: 'Message' }).fill('inspect the current runtime')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('.chat-bubble.chat-user')).toContainText('inspect the current runtime', { timeout: 250 })
  await expect(page.locator('.chat-bubble.chat-user .chat-bubble-footer time')).toHaveText('Just now')
  const userBubbleBox = await page.locator('.chat-bubble.chat-user').boundingBox()
  const transcriptBox = await page.locator('.chat-transcript').boundingBox()
  expect(userBubbleBox).not.toBeNull()
  expect(transcriptBox).not.toBeNull()
  expect(userBubbleBox!.width).toBeLessThan(transcriptBox!.width * 0.8)
  const streamingBubble = page.locator('.chat-bubble.chat-streaming')
	  const activityLabels = streamingBubble.locator('.chat-process-label')
	  await expect(activityLabels).toHaveText([
		'Thinking',
		'skill_view',
		'browser_navigate',
	  ])
	  const longHermesLabelLayout = await activityLabels.first().evaluate((element) => {
		const style = getComputedStyle(element)
		return {
			clientWidth: element.clientWidth,
			scrollWidth: element.scrollWidth,
			clientHeight: element.clientHeight,
			lineHeight: Number.parseFloat(style.lineHeight),
			whiteSpace: style.whiteSpace,
			textOverflow: style.textOverflow,
		}
	  })
	  expect(longHermesLabelLayout.whiteSpace).toBe('pre-wrap')
	  expect(longHermesLabelLayout.textOverflow).toBe('clip')
	  expect(longHermesLabelLayout.scrollWidth).toBeLessThanOrEqual(longHermesLabelLayout.clientWidth)
	  expect(longHermesLabelLayout.clientHeight).toBeGreaterThanOrEqual(longHermesLabelLayout.lineHeight)
	  await expect(streamingBubble.locator('.chat-process-card-meta')).toHaveText('3 activities')
	  await expect(streamingBubble.locator('.chat-process-card-title')).toHaveText('Waiting for Hermes')
	  await expect(streamingBubble.locator('.chat-process-card-heading .chat-process-fleet-icon')).toBeVisible()
	  await expect(streamingBubble.locator('.chat-process-card-heading .chat-process-hermes-icon')).toHaveCount(0)
	  await expect(streamingBubble.locator('.chat-process-live-label')).toBeVisible()
	  await expect(streamingBubble.locator('.chat-process-reasoning-text')).toHaveText('Inspecting the current Hermes session')
	  await expect(streamingBubble).not.toContainText('_thinking')
	  await expect(streamingBubble.locator('.chat-process-active-step .spin')).toBeVisible()
	  await expect(streamingBubble.locator('.chat-process-active-step .chat-process-hermes-icon img')).toHaveAttribute('src', '/hermes-logo.png')
	  await expect(streamingBubble.locator('.chat-process-active-label')).toHaveText('Thinking')
	  await expect(streamingBubble.locator('.chat-process-timeline .chat-process-hermes-icon')).toHaveCount(1)
	  await expect(streamingBubble.locator('.chat-process-content[title]')).toHaveCount(0)
	  await expect(streamingBubble).not.toContainText('upt posind3m4s e1')
	  await expect(streamingBubble.locator('.chat-process-arguments')).toHaveText([
		longSkillArgumentsText,
		'{"url":"https://example.test"}',
	  ])
	  const longArgumentsLayout = await streamingBubble.locator('.chat-process-arguments').first().evaluate((element) => {
		const style = getComputedStyle(element)
		return {
			clientHeight: element.clientHeight,
			scrollHeight: element.scrollHeight,
			lineHeight: Number.parseFloat(style.lineHeight),
			overflow: style.overflow,
			textOverflow: style.textOverflow,
			webkitLineClamp: style.webkitLineClamp,
		}
	  })
	  expect(longArgumentsLayout.webkitLineClamp).toBe('3')
	  expect(longArgumentsLayout.overflow).toBe('hidden')
	  expect(longArgumentsLayout.textOverflow).toBe('ellipsis')
	  expect(longArgumentsLayout.clientHeight).toBeLessThanOrEqual(Math.ceil(longArgumentsLayout.lineHeight * 3) + 1)
	  expect(longArgumentsLayout.scrollHeight).toBeGreaterThan(longArgumentsLayout.clientHeight)
	const backgroundItem = page.locator('.chat-session-item').filter({ hasText: 'Chat with background' })
	await expect(backgroundItem.locator('.chat-session-select')).toBeVisible()
	await expect(backgroundItem.locator('.chat-session-new')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Stop' })).toBeVisible()
  expect({ consoleErrors, failedResponses }).toEqual({ consoleErrors: [], failedResponses: [] })
  if (qaScreenshotPath) await page.screenshot({ path: qaScreenshotPath, fullPage: false })

  await page.getByRole('button', { name: `Delete ${session.title}` }).click()
  await page.getByRole('group', { name: `Confirm deletion of ${session.title}` }).getByRole('button', { name: /^(Delete|Stop & delete)$/ }).click()
  await expect(page.getByRole('button', { name: session.title })).toHaveCount(0)
  await expect(page.getByText('Create a session and choose its target instance.')).toBeVisible()
})

test('Codex setup remains distinct from runtime health but blocks fleet readiness', async ({ page }) => {
  const instance = codexInstance()
  let observationRefreshes = 0
  await openFleet(
    page,
    { hosts: [host], instances: [instance], operations: [] },
    { onObservationRefresh: () => { observationRefreshes += 1 } },
  )

  const attentionMetric = page.locator('.metric').filter({ hasText: 'Needs attention' })
  await expect(attentionMetric.locator('strong')).toHaveText('1')
  const row = page.getByRole('row').filter({ hasText: instance.name })
	await expect(row.getByRole('button', { name: 'View status details: Setup incomplete' })).toBeVisible()
  await expect(row.getByText('Needs attention', { exact: true })).toHaveCount(0)

  await page.getByRole('button', { name: instance.name }).click()
  await expect(page.locator('header.topbar p')).toHaveText('Running · Setup incomplete')
  await expect(page.getByText('Managed runtime is healthy')).toBeVisible()
  await expect(page.getByText('Setup incomplete', { exact: true })).toBeVisible()
  await expect(page.getByText('This setup is separate from runtime health.')).toBeVisible()
  await expect(page.locator('.instance-tabs .tab-count')).toHaveCount(0)

  await page.getByTitle('Refresh instance status').click()
  await expect.poll(() => observationRefreshes).toBe(1)
})

test('active lifecycle states remain checking while failed instances need attention', async ({ page }) => {
  const provisioning = {
    ...codexInstance(),
    id: 'instance-provisioning',
    name: 'fleet-provisioning',
    status: 'PROVISIONING',
    hermes_version: '0.20.0',
    hermes_version_verified: false,
    observation: undefined,
  }
  const failed = {
    ...codexInstance(),
    id: 'instance-failed',
    name: 'fleet-failed',
    status: 'FAILED',
    last_error: 'Dashboard readiness timed out',
    observation: undefined,
  }
  await openFleet(page, { hosts: [host], instances: [provisioning, failed], operations: [] })

  const attentionMetric = page.locator('.metric').filter({ hasText: 'Needs attention' })
  await expect(attentionMetric.locator('strong')).toHaveText('1')

  const provisioningRow = page.getByRole('row').filter({ hasText: provisioning.name })
  await expect(provisioningRow.getByRole('button', { name: 'View status details: Checking' })).toBeVisible()
  await expect(provisioningRow.getByText('Needs attention', { exact: true })).toHaveCount(0)

  const failedRow = page.getByRole('row').filter({ hasText: failed.name })
  await expect(failedRow.getByRole('button', { name: 'View status details: Needs attention' })).toBeVisible()
})

test('failed dashboard publication needs attention until the route is published', async ({ page }) => {
	const instance = {
		...codexInstance(),
		public_hostname: 'fleet-codex-sync.example.com',
	}
	const failedPublication = operation(0, {
		instance_id: instance.id,
		type: 'PUBLISH_DASHBOARD',
		status: 'FAILED',
		summary: `Publish dashboard ${instance.name}`,
		error: 'Public DNS is still propagating',
	})
	let published = false
	await page.route('**/api/v1/system/remote-access/configuration', async (route) => {
		await route.fulfill({ json: {
			mode: 'managed_cloudflare',
			admin_tunnel_token_configured: true,
			instances_tunnel_token_configured: true,
			instance_publishing_configured: true,
			legacy_provider_managed: false,
			instance_routes: [{
				instance_id: instance.id,
				instance_name: instance.name,
				hostname: instance.public_hostname,
				origin_service: 'http://hermes-fleet-instance-fleet-codex-sync-dashboard:9119',
				dns_state: 'ready',
				route_state: 'ready',
				endpoint_state: published ? 'reachable' : 'checking',
				provider_state: published ? 'published' : 'publishing',
				published,
			}],
		} })
	})
	await openFleet(
		page,
		{ hosts: [host], instances: [instance], operations: [failedPublication] },
		{ operations: [failedPublication] },
	)

	const attentionMetric = page.locator('.metric').filter({ hasText: 'Needs attention' })
	await expect(attentionMetric.locator('strong')).toHaveText('1')
	const row = page.getByRole('row').filter({ hasText: instance.name })
	await expect(row.getByRole('button', { name: 'View status details: Needs attention' })).toBeVisible()

	published = true
	await page.getByTitle('Reload Fleet data').click()
	await expect(attentionMetric.locator('strong')).toHaveText('1')
	await expect(row.getByRole('button', { name: 'View status details: Setup incomplete' })).toBeVisible()
})

test('recorded Hermes version replaces a stale observation while verification is pending', async ({ page }) => {
  const instance = {
    ...codexInstance(),
    hermes_version: '0.19.0',
    hermes_source: releases[0].commit,
    hermes_version_verified: false,
    observation: {
      ...codexInstance().observation,
      hermes_version: '0.18.2',
    },
  }
  await openFleet(page, { hosts: [host], instances: [instance], operations: [] })

  const row = page.getByRole('row').filter({ hasText: instance.name })
  await expect(row.getByText('0.19.0', { exact: true })).toBeVisible()
  await expect(row.getByText('Verifying', { exact: true })).toBeVisible()
  await expect(row.getByText('0.18.2', { exact: true })).toHaveCount(0)

  await page.getByRole('button', { name: instance.name }).click()
  const versionCard = page.locator('.overview-card').filter({ hasText: 'Hermes version' })
  await expect(versionCard.getByText('0.19.0', { exact: true })).toBeVisible()
  await expect(versionCard.getByText('Recorded version · verification pending')).toBeVisible()
})

test('Operations loads the dedicated history instead of the truncated overview', async ({ page }) => {
  const allOperations = Array.from({ length: 25 }, (_, index) => operation(index + 1))
  let historyRequests = 0
  await openFleet(page, {
    hosts: [host],
    instances: [],
    operations: allOperations.slice(0, 20),
  }, {
    operations: allOperations,
    onOperationsRequest: () => {
      historyRequests += 1
    },
  })

  await page.getByRole('button', { name: 'Operations' }).click()
  await expect.poll(() => historyRequests).toBeGreaterThan(0)
  await expect(page.getByText('25 operations shown')).toBeVisible()
  await page.getByRole('button', { name: 'Next' }).click()
  await page.getByRole('button', { name: 'Next' }).click()
  await expect(page.getByText('Operation 25', { exact: true })).toBeVisible()
})

test('Operations centralizes filters, active status, and staged audit details', async ({ page }) => {
	const instance = codexInstance('model-alpha')
	const operations = [
		operation(1, {
			id: 'operation-running', instance_id: instance.id, type: 'PUBLISH_DASHBOARD', status: 'RUNNING', summary: 'Publishing dashboard',
			progress: { stage: 'UPDATING_INGRESS', detail: 'Updating Cloudflare ingress', steps: [
				{ stage: 'VALIDATING_HOSTNAME', status: 'succeeded' },
				{ stage: 'UPDATING_INGRESS', status: 'running', detail: 'Updating Cloudflare ingress' },
			] },
		}),
		operation(2, {
			id: 'operation-failed', instance_id: instance.id, type: 'PUBLISH_DASHBOARD', status: 'FAILED', summary: 'Publishing previous hostname',
			error: 'Cloudflare rejected the route', progress: { stage: 'UPDATING_INGRESS', action_code: 'replace_api_token' },
		}),
	]
	await page.route(`**/api/v1/instances/${instance.id}/recovery-points`, async (route) => route.fulfill({ json: [] }))
	await openFleet(page, { hosts: [host], instances: [instance], operations }, { operations })

	await expect(page.locator('.nav-count')).toHaveText('1')
	await page.getByLabel('Primary navigation').getByRole('button', { name: 'Operations', exact: true }).click()
	await expect(page.getByLabel('Filter operations by status')).toHaveValue('ACTIVE')
	const summary = page.getByLabel('Operation status summary')
	await expect(summary).toContainText('Active1')
	await expect(summary).toContainText('Failed1')
	await expect(page.getByLabel('Filter operations by instance')).toHaveValue('ALL')
	await page.getByLabel('Filter operations by status').selectOption('FAILED')
	await expect(page.getByText('1 matching of 2')).toBeVisible()
	await page.getByRole('button', { name: /Publishing previous hostname/ }).click()
	const detail = page.getByRole('dialog', { name: 'Operation details' })
	await expect(detail).toBeVisible()
	await expect(detail.getByText('Cloudflare rejected the route')).toBeVisible()
	await expect(detail.getByText(/Required action: Replace api token/)).toBeVisible()
	await expect(detail.getByRole('button', { name: /Retry/ })).toHaveCount(0)
	await page.keyboard.press('Escape')
	await expect(detail).toBeHidden()

	await page.getByRole('button', { name: 'Instances' }).click()
	await page.getByRole('button', { name: instance.name }).click()
	await page.getByLabel('Instance modules').getByRole('button', { name: 'Operations', exact: true }).click()
	await expect(page.getByLabel('Filter operations by instance')).toHaveCount(0)
	await expect(page.getByLabel('Filter operations by type')).toBeVisible()
})

test('Operations follows the opaque cursor to load older history', async ({ page }) => {
  const allOperations = Array.from({ length: 55 }, (_, index) => operation(index + 1))
  let olderRequests = 0
  await openFleet(page, {
    hosts: [host],
    instances: [],
    operations: allOperations.slice(0, 20),
  }, {
    operations: allOperations.slice(0, 50),
    olderOperations: allOperations.slice(50),
    onOlderOperationsRequest: () => {
      olderRequests += 1
    },
  })

  await page.getByRole('button', { name: 'Operations' }).click()
  await expect(page.getByText(/50 operations shown · older history available/)).toBeVisible()
  await page.getByRole('button', { name: 'Load older' }).click()
  await expect.poll(() => olderRequests).toBe(1)
  await expect(page.getByText(/55 operations shown/)).toBeVisible()
  await page.getByLabel('Search operations').fill('Operation 55')
  await expect(page.getByText('Operation 55', { exact: true })).toBeVisible()

  await page.getByTitle('Reload Fleet data').click()
  await expect(page.getByText(/1 matching of 55 operations/)).toBeVisible()
  await expect(page.getByText('Operation 55', { exact: true })).toBeVisible()
  expect(olderRequests).toBe(1)
})

test('Operations can stop an active chat response from its detail panel', async ({ page }) => {
	const activeChatOperation = operation(1, {
		type: 'CHAT_MESSAGE',
		status: 'RUNNING',
		summary: 'Send chat message to fleet-test-01',
		metadata: { chat_session_id: 'session-active-chat', chat_message_id: 'message-active-chat' },
	})
	let cancelRequests = 0
	await page.route('**/api/v1/chats/session-active-chat/cancel', async (route) => {
		cancelRequests += 1
		await route.fulfill({ status: 204, body: '' })
	})
	await openFleet(
		page,
		{ hosts: [host], instances: [], operations: [activeChatOperation] },
		{ operations: [activeChatOperation] },
	)

	await page.getByRole('button', { name: 'Operations', exact: true }).click()
	await page.getByRole('button', { name: /Send chat message to fleet-test-01/ }).click()
	const detail = page.getByRole('dialog', { name: 'Operation details' })
	await detail.getByRole('button', { name: 'Stop response' }).click()
	await expect.poll(() => cancelRequests).toBe(1)
})

test('Alerts derives active backup risk and incident history from authoritative sources', async ({ page }) => {
	const failedOperation = operation(2, { type: 'REPAIR_RUNTIME', status: 'FAILED', summary: 'Restart managed runtime', error: 'Runtime health check failed' })
	const successfulOperation = operation(1, { type: 'REPAIR_RUNTIME', status: 'SUCCEEDED', summary: 'Restart managed runtime' })
  await page.route('**/api/v1/system/runtime-health', async (route) => route.fulfill({ json: {
    status: 'healthy', stream_id: 'alerts-stream', state_revision: 4, event_subscribers: 1,
    compatibility: { control_plane_version: '0.11.0', host_agent_version: host.agent_version, runtime_config_schemas: [1], default_job_concurrency: 1, maximum_job_concurrency: 4, features: [] },
    queue: { pending: 0, active: 0, expired_leases: 0, admission_rejected: false, max_per_host: 4, hosts: [{ host_id: host.id, host_name: host.name, pending: 0, active: 0, expired_leases: 0, admission_open: true }] },
    metrics: { started_at: now, uptime_seconds: 60, http_requests: 10, http_failures: 0, http_in_flight: 0, duration_samples: 10, average_http_ms: 1, p95_http_ms: 2, p99_http_ms: 3, max_http_ms: 4, mutations: 1, queue_rejected: 0, jobs_reconciled: 0 },
    components: [{ component: 'control_plane', status: 'healthy', detail: 'ready', updated_at: now, last_success_at: now }],
		recent_incidents: [
			{ id: 9, component: 'remote_access', previous_status: 'degraded', status: 'healthy', detail: 'synced', occurred_at: now },
			{ id: 8, component: 'remote_access', previous_status: 'degraded', status: 'healthy', detail: 'synced', occurred_at: '2026-07-26T23:55:00Z' },
			{ id: 7, component: 'remote_access', previous_status: 'degraded', status: 'healthy', detail: 'synced', occurred_at: '2026-07-26T23:50:00Z' },
		],
  } }))
  await page.route('**/api/v1/system', async (route) => route.fulfill({ json: {
    fleet_version: '0.11.0', build_id: 'alerts-build', operator_url: 'http://127.0.0.1:9180', database_path: '/var/lib/hermes-fleet/fleet.db', backup_retention: 2,
    readiness: { ready: true, database: 'ready', storage: 'ready', release_catalog: 'ready', capacity: { free_bytes: 10, total_bytes: 20, free_percent: 50, free_inodes: 10, minimum_free_bytes: 1, minimum_free_percent: 5, minimum_free_inodes: 1, operations_safe: true }, last_checked: now },
    capacity: { free_bytes: 10, total_bytes: 20, free_percent: 50, free_inodes: 10, minimum_free_bytes: 1, minimum_free_percent: 5, minimum_free_inodes: 1, operations_safe: true },
    capabilities: { control_plane_version: '0.11.0', host_agent_version: host.agent_version, runtime_config_schemas: [1], default_job_concurrency: 1, maximum_job_concurrency: 4, features: [] },
    recovery_drill: { status: 'NEVER_RUN', control_plane_backup_checked: false, instance_backups_checked: 0, instances_without_backup: 0 },
    remote_access: { configured: true, state: 'synced', admin: { routes: 1, synced: true }, instances: { routes: 1, synced: true } },
  } }))
  await page.route('**/api/v1/backups', async (route) => route.fulfill({ json: [
    { id: 'backup-2', filename: 'backup-2.sqlite', size_bytes: 100, sha256: 'b'.repeat(64), created_at: now, verified_at: now },
    { id: 'backup-1', filename: 'backup-1.sqlite', size_bytes: 100, sha256: 'a'.repeat(64), created_at: now, verified_at: now },
  ] }))
	await openFleet(page, { hosts: [host], instances: [], operations: [successfulOperation, failedOperation] }, { operations: [successfulOperation, failedOperation] })

  const alertsNav = page.getByRole('button', { name: 'Alerts', exact: true })
  await alertsNav.click()
	await expect(alertsNav.locator('.alert-nav-count')).toHaveCount(0)
  const summary = page.getByLabel('Alert summary')
	await expect(summary).toContainText('Active0')
	await expect(summary).toContainText('Warning0')
	await expect(summary).toContainText('Recent incidents2')
	await expect(page.getByRole('button', { name: 'Open alert: Remote access recovered' })).toHaveCount(1)
	await expect(page.getByRole('button', { name: 'Open alert: Remote access recovered' })).toContainText('Occurred 3 times')
	await expect(page.getByRole('button', { name: 'Open alert: Restart managed runtime' })).toContainText('Superseded')
	await expect(page.getByRole('button', { name: 'Open alert: Backup retention is full' })).toHaveCount(0)
})

test('Alerts preserves available evidence when one source is unavailable', async ({ page }) => {
  const failedOperation = operation(1, { status: 'FAILED', summary: 'Repair instance runtime', error: 'Container health check failed' })
  await page.route('**/api/v1/system/runtime-health', async (route) => route.fulfill({ json: {
    status: 'healthy', stream_id: 'partial-alert-stream', state_revision: 2, event_subscribers: 1,
    compatibility: { control_plane_version: '0.11.0', host_agent_version: host.agent_version, runtime_config_schemas: [1], default_job_concurrency: 1, maximum_job_concurrency: 4, features: [] },
    queue: { pending: 0, active: 0, expired_leases: 0, admission_rejected: false, max_per_host: 4, hosts: [] },
    metrics: { started_at: now, uptime_seconds: 60, http_requests: 10, http_failures: 0, http_in_flight: 0, duration_samples: 10, average_http_ms: 1, p95_http_ms: 2, p99_http_ms: 3, max_http_ms: 4, mutations: 1, queue_rejected: 0, jobs_reconciled: 0 },
    components: [], recent_incidents: [],
  } }))
  await page.route('**/api/v1/system', async (route) => route.fulfill({ json: {
    fleet_version: '0.11.0', build_id: 'partial-alert-build', operator_url: 'http://127.0.0.1:9180', database_path: '/var/lib/hermes-fleet/fleet.db', backup_retention: 20,
  } }))
  await page.route('**/api/v1/backups', async (route) => route.fulfill({ status: 503, json: { error: 'backup storage unavailable' } }))
  await openFleet(page, { hosts: [host], instances: [], operations: [failedOperation] }, { operations: [failedOperation] })

  await page.getByRole('button', { name: 'Alerts', exact: true }).click()
  await expect(page.getByText('Some alert sources are unavailable')).toBeVisible()
  await expect(page.getByText(/Control-plane backups:/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Open alert: Repair instance runtime' })).toBeVisible()
  await expect(page.getByLabel('Loading alert sources')).toHaveCount(0)
})

test('a recovered workflow reports the latest terminal attempt', async ({ page }) => {
  const workflowID = '00000000-0000-4000-8000-000000000501'
  const workflowOperations = [
    operation(2, {
      id: 'repair-attempt-1',
      workflow_id: workflowID,
      type: 'REPAIR_RUNTIME',
      status: 'FAILED',
      summary: 'Repair managed runtime',
      metadata: { attempt: 1 },
      error: 'Container did not become healthy',
    }),
    operation(1, {
      id: 'repair-attempt-2',
      workflow_id: workflowID,
      type: 'REPAIR_RUNTIME',
      status: 'SUCCEEDED',
      summary: 'Repair managed runtime',
      metadata: { attempt: 2 },
    }),
  ]
  await openFleet(
    page,
    { hosts: [host], instances: [], operations: workflowOperations },
    { operations: workflowOperations },
  )

  await page.getByRole('button', { name: 'Operations' }).click()
  const row = page.getByRole('row').filter({ hasText: 'Repair managed runtime' })
  await expect(row.locator('td[data-label="Status"]').getByText('Succeeded')).toBeVisible()
  await expect(row.locator('td[data-label="Status"]').getByText('Failed')).toHaveCount(0)
})

test('top Refresh reloads the active System section', async ({ page }) => {
  let systemReads = 0
  let backupReads = 0
  await page.route('**/api/v1/system', async (route) => {
    systemReads += 1
    await route.fulfill({
      json: {
        fleet_version: systemReads === 1 ? '0.10.0' : '0.10.1',
        build_id: `build-${systemReads}`,
        operator_url: 'http://127.0.0.1:9180',
        database_path: '/var/lib/hermes-fleet/fleet.db',
        backup_retention: 20,
        remote_access: {
          configured: false,
          state: 'disabled',
          admin: { routes: 0, synced: false },
          instances: { routes: 0, synced: false },
        },
      },
    })
  })
  await page.route('**/api/v1/backups', async (route) => {
    backupReads += 1
    const suffix = backupReads === 1 ? 'before' : 'after'
    await route.fulfill({
      json: [{
        id: `backup-${suffix}`,
        filename: `fleet-${suffix}.sqlite`,
        size_bytes: 1024,
        sha256: suffix.repeat(64).slice(0, 64),
        created_at: now,
        verified_at: now,
      }],
    })
  })
  await openFleet(page, { hosts: [host], instances: [], operations: [] })

  await page.getByRole('button', { name: 'System' }).click()
  await expect(page.getByText('Version 0.10.0')).toBeVisible()
  await page.getByTitle('Reload Fleet data').click()
  await expect(page.getByText('Version 0.10.1')).toBeVisible()
  expect(systemReads).toBeGreaterThanOrEqual(2)

  await page.getByRole('button', { name: 'Backups & recovery' }).click()
  await expect(page.getByText('fleet-before.sqlite')).toBeVisible()
  await page.getByTitle('Reload Fleet data').click()
  await expect(page.getByText('fleet-after.sqlite')).toBeVisible()
  expect(backupReads).toBeGreaterThanOrEqual(2)
})

test('Cloudflare remote access reports connector readiness without claiming provider route verification', async ({ page }) => {
  let reconciles = 0
	await page.route('**/api/v1/system/remote-access/configuration', async (route) => {
		await route.fulfill({ json: {
			mode: 'managed_cloudflare',
			admin_hostname: 'adminhermesfleet.example.com',
			admin_tunnel_token_configured: true, instances_tunnel_token_configured: true, legacy_provider_managed: false,
			admin_tunnel_token_fingerprint: 'A1B2C3D4E5', instances_tunnel_token_fingerprint: 'F6E7D8C9B0',
			instance_publishing_configured: true, instance_publishing_account_id: 'account-id',
			instance_publishing_zone_id: 'zone-id', instance_publishing_zone: 'example.com',
			instance_publishing_tunnel_id: instancesTunnelID, instance_publishing_fleet_namespace: 'fleet',
			instance_publishing_token_fingerprint: '1122334455', instance_routes: [{
				instance_id: 'instance-aksa', instance_name: 'aksa', hostname: 'aksa.example.com',
				origin_service: 'http://hermes-fleet-instance-aksa-dashboard:9119',
				dns_state: 'ready', dns_detail: 'Cloudflare DNS CNAME is owned by Fleet and targets this tunnel',
				route_state: 'ready', route_detail: 'Cloudflare ingress is owned by Fleet and matches the service URL',
				endpoint_state: 'reachable', endpoint_detail: 'Public endpoint returned redirect HTTP 302',
				provider_state: 'published', published: true,
			}],
		} })
	})
  await page.route('**/api/v1/system/remote-access/reconcile', async (route) => {
    reconciles += 1
    await route.fulfill({ status: 202, json: {} })
  })
  await page.route('**/api/v1/system', async (route) => {
    await route.fulfill({
      json: {
        fleet_version: '0.10.0',
        build_id: 'remote-access-test',
        operator_url: 'http://127.0.0.1:9180',
        database_path: '/var/lib/hermes-fleet/fleet.db',
        backup_retention: 20,
		remote_access: {
			configured: true, mode: 'managed_cloudflare',
          state: 'synced',
          admin: {
	            tunnel_id: adminTunnelID,
            hostname: 'adminhermesfleet.example.com',
            routes: 1,
            synced: true,
          },
          instances: {
	            tunnel_id: instancesTunnelID,
            routes: 2,
            synced: true,
          },
          last_sync_at: now,
        },
      },
    })
  })
  await openFleet(page, { hosts: [host], instances: [], operations: [] })

  await page.getByRole('button', { name: 'System' }).click()
  await page.getByRole('button', { name: 'Remote access' }).click()
	await expect(page.getByText('Connectors ready', { exact: true })).toBeVisible()
	await expect(page.getByText('adminhermesfleet.example.com')).toBeVisible()
	await expect(page.getByText('2 managed instances in example.com.')).toBeVisible()
	await page.getByText('Technical details', { exact: true }).click()
	await expect(page.getByText('Waiting for connector health checks')).toBeVisible()
	await expect(page.getByText(/Admin not checked · Instances not checked/)).toBeVisible()
	const routeRow = page.getByRole('row').filter({ hasText: 'aksa.example.com' })
	await expect(routeRow.getByText('Cloudflare DNS CNAME is owned by Fleet and targets this tunnel')).not.toBeVisible()
	await expect(routeRow.getByText('Cloudflare ingress is owned by Fleet and matches the service URL')).not.toBeVisible()
	await expect(routeRow.getByText('Public endpoint returned redirect HTTP 302')).not.toBeVisible()
	await expect(routeRow.locator('td[data-label="DNS"]')).toHaveAttribute('title', 'Cloudflare DNS CNAME is owned by Fleet and targets this tunnel')
	const workflow = page.getByRole('list', { name: 'Cloudflare route publication workflow' })
	await expect(page.getByText('How publishing works', { exact: true })).toBeVisible()
	await expect(workflow).not.toBeVisible()
	await page.getByText('How publishing works', { exact: true }).click()
	await expect(workflow).toBeVisible()
	const disableRemoteAccess = page.getByRole('button', { name: 'Disable remote access' })
	await expect(disableRemoteAccess).toHaveClass(/secondary-button/)
	expect(await disableRemoteAccess.evaluate((element) => element.getBoundingClientRect().height)).toBeGreaterThanOrEqual(38)
	await page.getByRole('button', { name: 'Edit configuration' }).click()
	await expect(page.getByLabel('Stored tunnel token')).toHaveCount(2)
	await expect(page.getByText('ID A1B2C3D4E5')).toBeVisible()
	await expect(page.getByText('ID F6E7D8C9B0')).toBeVisible()
	await expect(page.getByText(/Stored encrypted by Fleet/)).toHaveCount(3)
	await expect(page.locator('.published-route-list')).toHaveCount(0)
	await page.getByRole('button', { name: 'Replace token' }).first().click()
	await expect(page.getByLabel('New tunnel token')).toBeVisible()
	await page.getByRole('button', { name: 'Cancel replacement' }).click()
	await expect(page.getByLabel('Stored tunnel token')).toHaveCount(2)
	await page.getByRole('button', { name: 'Cancel' }).click()
	await page.getByRole('button', { name: 'Check connectors' }).click()
  await expect.poll(() => reconciles).toBe(1)
})

test('Cloudflare cleanup failure remains explicit and retryable', async ({ page }) => {
	let configured = true
	let cleanupRetries = 0
	await page.route('**/api/v1/system/remote-access/configuration', async (route) => {
		if (route.request().method() === 'DELETE') {
			cleanupRetries += 1
			configured = false
			await route.fulfill({ json: { configured: false, state: 'disabled', admin: { routes: 0, synced: false }, instances: { routes: 0, synced: false } } })
			return
		}
		await route.fulfill({ json: configured ? {
			mode: 'managed_cloudflare', admin_tunnel_id: adminTunnelID, instances_tunnel_id: instancesTunnelID,
			admin_hostname: 'admin.example.com',
			admin_tunnel_token_configured: false, instances_tunnel_token_configured: false, legacy_provider_managed: true,
		} : { mode: '', admin_tunnel_token_configured: false, instances_tunnel_token_configured: false, legacy_provider_managed: false } })
	})
	await page.route('**/api/v1/system', async (route) => {
		await route.fulfill({ json: {
			fleet_version: '0.10.0', build_id: 'cleanup-test', operator_url: 'http://127.0.0.1:9180',
			database_path: '/var/lib/hermes-fleet/fleet.db', backup_retention: 20,
			remote_access: configured ? {
				configured: true, mode: 'managed_cloudflare', state: 'cleanup_pending', last_error: 'Cloudflare API unavailable',
				admin: { hostname: 'admin.example.com', routes: 0, synced: false },
				instances: { routes: 0, synced: false },
			} : { configured: false, state: 'disabled', admin: { routes: 0, synced: false }, instances: { routes: 0, synced: false } },
		} })
	})
	await openFleet(page, { hosts: [host], instances: [], operations: [] })
	await page.getByRole('button', { name: 'System' }).click()
	await page.getByRole('button', { name: 'Remote access' }).click()
	await expect(page.getByText('Cleanup incomplete', { exact: true })).toBeVisible()
	await expect(page.getByText('Legacy configuration is active', { exact: true })).toBeVisible()
	await expect(page.getByRole('button', { name: 'Edit configuration' })).toHaveCount(0)
	await expect(page.getByRole('button', { name: 'Check connectors' })).toHaveCount(0)
	page.once('dialog', (dialog) => void dialog.accept())
	await page.getByRole('button', { name: 'Retry cleanup' }).click()
	await expect.poll(() => cleanupRetries).toBe(1)
	await expect(page.locator('.remote-access-editor')).toBeVisible()
})

test('Cloudflare remote access is configured only through the Fleet web flow', async ({ page }) => {
	const instance = codexInstance()
	let savedAdmin: Record<string, string> | null = null
	let savedPublishing: Record<string, string> | null = null
	let adminConfigured = false
	let publishingConfigured = false
	await page.route('**/api/v1/system/remote-access/configuration', async (route) => {
		await route.fulfill({ json: {
			mode: adminConfigured || publishingConfigured ? 'managed_cloudflare' : '',
			admin_hostname: adminConfigured ? 'admin.example.com' : '',
			admin_tunnel_token_configured: adminConfigured,
			instances_tunnel_token_configured: publishingConfigured,
			admin_tunnel_token_fingerprint: adminConfigured ? 'A1B2C3D4E5' : '',
			instances_tunnel_token_fingerprint: publishingConfigured ? 'F6E7D8C9B0' : '',
			instance_publishing_configured: publishingConfigured,
			instance_publishing_account_id: publishingConfigured ? 'account-id' : '',
			instance_publishing_zone_id: publishingConfigured ? 'zone-id' : '',
			instance_publishing_zone: publishingConfigured ? 'example.com' : '',
			instance_publishing_tunnel_id: publishingConfigured ? instancesTunnelID : '',
			instance_publishing_token_fingerprint: publishingConfigured ? '1122334455' : '',
			legacy_provider_managed: false,
			admin_origin_service: 'http://control-plane:9180',
			instance_routes: [{
				instance_id: instance.id,
				instance_name: instance.name,
				hostname: '',
				origin_service: 'http://hermes-fleet-instance-fleet-codex-sync-dashboard:9130',
				dns_state: 'pending', route_state: 'pending', endpoint_state: 'unchecked', published: false,
			}],
		} })
	})
	await page.route('**/api/v1/system/remote-access/cloudflare/admin', async (route) => {
		savedAdmin = route.request().postDataJSON()
		adminConfigured = true
		await route.fulfill({ json: {} })
	})
	await page.route('**/api/v1/system/remote-access/cloudflare/instance-publishing', async (route) => {
		savedPublishing = route.request().postDataJSON()
		publishingConfigured = true
		await route.fulfill({ status: 202, json: operation(901, { id: 'operation-publishing', instance_id: '', type: 'CONNECT_INSTANCE_PUBLISHING', status: 'PENDING', summary: 'Connect and verify instance publishing' }) })
	})
	await page.route('**/api/v1/operations/operation-publishing', async (route) => {
		await route.fulfill({ json: operation(901, { id: 'operation-publishing', instance_id: '', type: 'CONNECT_INSTANCE_PUBLISHING', status: 'SUCCEEDED', summary: 'Connect and verify instance publishing' }) })
	})
	await page.route('**/api/v1/system', async (route) => {
		const configured = adminConfigured || publishingConfigured
		await route.fulfill({ json: {
			fleet_version: '0.10.0', build_id: 'remote-web-test', operator_url: 'http://127.0.0.1:9180',
			database_path: '/var/lib/hermes-fleet/fleet.db', backup_retention: 20,
			remote_access: configured
				? { configured: true, mode: 'managed_cloudflare', state: publishingConfigured ? 'synced' : 'pending', admin: { hostname: 'admin.example.com', routes: 0, synced: adminConfigured }, instances: { tunnel_id: instancesTunnelID, routes: 0, synced: publishingConfigured } }
				: { configured: false, state: 'disabled', admin: { routes: 0, synced: false }, instances: { routes: 0, synced: false } },
		} })
	})
	await openFleet(page, { hosts: [host], instances: [instance], operations: [] })
	await page.getByRole('button', { name: 'System' }).click()
	await page.getByRole('button', { name: 'Remote access' }).click()

	const editor = page.locator('.remote-access-editor')
	await expect(editor).toBeVisible()
	await expect(page.getByText(/FLEET_CLOUDFLARE|setup-cloudflare/)).toHaveCount(0)
	await editor.getByLabel('Cloudflare tunnels').check()
	const adminConnectorToken = `eyJ${'A'.repeat(256)}`
	const instancesConnectorToken = `eyJ${'B'.repeat(256)}`
	const cards = editor.locator('.remote-access-boundary-card')
	const adminCard = cards.nth(0)
	const instancesCard = cards.nth(1)
	await adminCard.getByLabel('Tunnel token').fill(adminConnectorToken)
	expect(await adminCard.getByLabel('Tunnel token').getAttribute('type')).toBe('text')
	await expect(adminCard.getByLabel('Tunnel token')).toHaveValue(adminConnectorToken)
	expect(await adminCard.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true)
	await adminCard.getByLabel('Public hostname').fill('admin.example.com')
	await adminCard.getByRole('button', { name: 'Save admin tunnel' }).click()
	await expect.poll(() => savedAdmin).toMatchObject({ tunnel_token: adminConnectorToken, hostname: 'admin.example.com' })
	await expect(adminCard.getByLabel('Stored tunnel token')).toBeVisible()
	await expect(adminCard.getByText('ID A1B2C3D4E5')).toBeVisible()
	await expect(adminCard.getByRole('textbox', { name: 'Public hostname' })).toHaveCount(0)
	await expect(adminCard.getByLabel('Saved public hostname')).toHaveText('admin.example.com')
	await adminCard.getByRole('button', { name: 'Change hostname' }).click()
	await expect(adminCard.getByRole('textbox', { name: 'Public hostname' })).toHaveValue('admin.example.com')
	await adminCard.getByRole('textbox', { name: 'Public hostname' }).fill('admin-new.example.com')
	savedAdmin = null
	await adminCard.getByRole('button', { name: 'Save admin tunnel' }).click()
	await expect.poll(() => savedAdmin).toMatchObject({ tunnel_token: '', hostname: 'admin-new.example.com' })
	await expect(adminCard.getByRole('textbox', { name: 'Public hostname' })).toHaveCount(0)
	await expect(adminCard.getByLabel('Saved public hostname')).toHaveText('admin.example.com')

	await instancesCard.getByLabel('Tunnel token').fill(instancesConnectorToken)
	await expect(instancesCard.getByLabel('Tunnel token')).toHaveValue(instancesConnectorToken)
	await instancesCard.getByLabel('Cloudflare Account ID').fill('account-id')
	await instancesCard.getByLabel('Zone ID').fill('zone-id')
	await instancesCard.getByLabel('Cloudflare API token').fill('api-token-secret')
	await instancesCard.getByLabel('Fleet namespace').fill('fleet')
	// Pending remote-access polling must refresh server status without replacing unsaved form drafts.
	await page.waitForTimeout(2200)
	await expect(instancesCard.getByLabel('Tunnel token')).toHaveValue(instancesConnectorToken)
	await expect(instancesCard.getByLabel('Cloudflare Account ID')).toHaveValue('account-id')
	await expect(instancesCard.getByLabel('Zone ID')).toHaveValue('zone-id')
	await expect(instancesCard.getByLabel('Cloudflare API token')).toHaveValue('api-token-secret')
	await expect(instancesCard.getByLabel('Fleet namespace')).toHaveValue('fleet')
	await expect(instancesCard.getByText('Instance publishing inventory', { exact: true })).toBeVisible()
	await instancesCard.getByRole('button', { name: 'Connect and verify' }).click()
	await expect.poll(() => savedPublishing).toMatchObject({ tunnel_token: instancesConnectorToken, account_id: 'account-id', zone_id: 'zone-id', api_token: 'api-token-secret', fleet_namespace: 'fleet' })
	await expect(page.getByText('Connectors ready', { exact: true })).toBeVisible()
	await expect(page.getByText('Tunnel tokens are encrypted by Fleet and never returned through the API.')).toBeVisible()
	await page.getByRole('button', { name: 'Edit configuration' }).click()
	const reopenedInstancesCard = page.locator('.remote-access-editor').locator('.remote-access-boundary-card').nth(1)
	await expect(reopenedInstancesCard.getByText('ID F6E7D8C9B0')).toBeVisible()
	await expect(reopenedInstancesCard.getByText('ID 1122334455')).toBeVisible()
	await expect(reopenedInstancesCard.getByText(instancesTunnelID, { exact: true })).toBeVisible()
	await expect(reopenedInstancesCard.getByText('example.com', { exact: true })).toBeVisible()
})

test('instance publishing shows staged failure and never claims Published early', async ({ page }) => {
	const instance = { ...codexInstance(), public_hostname: '' }
	let submittedHostname = ''
	await page.route('**/api/v1/instances/*/recovery-points', async (route) => route.fulfill({ json: [] }))
	await page.route('**/api/v1/system/remote-access/configuration', async (route) => {
		await route.fulfill({ json: {
			mode: 'managed_cloudflare', admin_tunnel_token_configured: true, instances_tunnel_token_configured: true,
			instance_publishing_configured: true, instance_publishing_account_id: 'account-id',
			instance_publishing_zone_id: 'zone-id', instance_publishing_zone: 'example.com', instance_publishing_tunnel_id: instancesTunnelID,
			instance_publishing_fleet_namespace: 'fleet',
			legacy_provider_managed: false,
			instance_routes: [{
				instance_id: instance.id, instance_name: instance.name, hostname: submittedHostname,
				origin_service: 'http://hermes-fleet-instance-fleet-codex-sync-dashboard:9119',
				dns_state: submittedHostname ? 'ready' : 'pending', route_state: submittedHostname ? 'failed' : 'pending',
				endpoint_state: 'unchecked', provider_state: submittedHostname ? 'failed' : 'ready_to_publish', published: false,
			}],
		} })
	})
	await page.route('**/api/v1/instances/*/public-dashboard', async (route) => {
		submittedHostname = route.request().postDataJSON().public_hostname
		await route.fulfill({ status: 202, json: operation(902, {
			id: 'operation-publish-dashboard', instance_id: instance.id, type: 'PUBLISH_DASHBOARD', status: 'PENDING',
			summary: `Publishing ${submittedHostname}`,
		}) })
	})
	await page.route('**/api/v1/operations/operation-publish-dashboard', async (route) => {
		await route.fulfill({ json: operation(902, {
			id: 'operation-publish-dashboard', instance_id: instance.id, type: 'PUBLISH_DASHBOARD', status: 'FAILED',
			summary: `Publishing ${submittedHostname}`, error: 'Cloudflare API token cannot edit tunnel configuration.',
			progress: {
				stage: 'UPDATING_INGRESS', detail: 'Cloudflare API token cannot edit tunnel configuration.', action_code: 'replace_api_token',
				steps: [
					{ stage: 'VALIDATING_HOSTNAME', status: 'succeeded' },
					{ stage: 'CREATING_DNS', status: 'succeeded' },
					{ stage: 'UPDATING_INGRESS', status: 'failed', detail: 'Cloudflare API token cannot edit tunnel configuration.' },
					{ stage: 'VERIFYING_CLOUDFLARE', status: 'pending' },
					{ stage: 'CHECKING_PUBLIC_ENDPOINT', status: 'pending' },
				],
			},
		}) })
	})
	await openFleet(page, { hosts: [host], instances: [instance], operations: [] })
	await expect(page.getByRole('cell', { name: 'Not configured', exact: true })).toBeVisible()
	await page.getByRole('button', { name: instance.name }).click()
	await page.getByRole('button', { name: 'Access' }).click()
	await page.getByText('Technical details', { exact: true }).click()
	await expect(page.getByText('http://hermes-fleet-instance-fleet-codex-sync-dashboard:9119', { exact: true })).toBeVisible()
	await expect(page.getByRole('button', { name: 'Copy Service URL' })).toBeVisible()
	await expect(page.getByText('fleet-fleet-codex-sync.example.com', { exact: true })).toBeVisible()
	await expect(page.getByText('Generated by Fleet from its namespace, instance name, and verified zone.')).toBeVisible()
	await page.getByRole('button', { name: 'Publish dashboard' }).click()
	await expect.poll(() => submittedHostname).toBe('fleet-fleet-codex-sync.example.com')
	await expect(page.getByText('Cloudflare API token cannot edit tunnel configuration.', { exact: true }).first()).toBeVisible()
	await expect(page.getByText('Create or update DNS', { exact: true })).toBeVisible()
	await expect(page.getByText('Create or update tunnel ingress', { exact: true })).toBeVisible()
	await expect(page.getByRole('link', { name: 'Replace API token' })).toHaveAttribute('href', '#system/remote-access')
	await expect(page.getByRole('link', { name: 'Remote access settings' })).toHaveAttribute('href', '#system/remote-access')
	await expect(page.getByText('Published', { exact: true })).toHaveCount(0)
})

test('existing public endpoints remain provider-neutral and drive Fleet navigation', async ({ page }) => {
	const instance = codexInstance('model-alpha')
	let savedConfiguration: Record<string, unknown> | null = null
	await page.route('**/api/v1/instances', async (route) => {
		await route.fulfill({ json: [instance] })
	})
	await page.route('**/api/v1/system/remote-access/configuration', async (route) => {
		if (route.request().method() === 'PUT') {
			savedConfiguration = route.request().postDataJSON()
			await route.fulfill({ json: {
				configured: true, mode: 'existing_endpoints', state: 'registered',
				admin: { url: 'https://admin.example.com', routes: 1, synced: false },
				instances: { routes: 1, synced: false },
			} })
			return
		}
		await route.fulfill({ json: {
			mode: 'existing_endpoints', admin_url: 'https://admin.example.com',
			instance_endpoints: [{ instance_id: instance.id, instance_name: instance.name, dashboard_url: 'https://agent.example.com' }],
			admin_credential_available: false, instances_credential_available: false, legacy_provider_managed: false,
		} })
	})
	await page.route('**/api/v1/system', async (route) => {
		await route.fulfill({ json: {
			fleet_version: '0.11.0', build_id: 'external-endpoints-test', operator_url: 'http://127.0.0.1:9180',
			database_path: '/var/lib/hermes-fleet/fleet.db', backup_retention: 20,
			remote_access: {
				configured: true, mode: 'existing_endpoints', state: 'registered',
				admin: { url: 'https://admin.example.com', routes: 1, synced: false },
				instances: { routes: 1, synced: false },
			},
		} })
	})
	await openFleet(page, { hosts: [host], instances: [instance], operations: [] })
	await page.getByRole('button', { name: 'System' }).click()
	await page.getByRole('button', { name: 'Remote access' }).click()

	await expect(page.getByText('Endpoints registered', { exact: true })).toBeVisible()
	await expect(page.getByRole('link', { name: 'https://admin.example.com' })).toBeVisible()
	await expect(page.getByText('Fleet never creates, updates, verifies, or removes provider routes')).toBeVisible()
	await expect(page.getByRole('button', { name: 'Reconcile ingress' })).toHaveCount(0)

	await page.getByRole('button', { name: 'Edit configuration' }).click()
	await expect(page.getByLabel('Existing public endpoints')).toBeChecked()
	await expect(page.getByLabel('Existing public endpoints')).toBeDisabled()
	await page.getByLabel(`${instance.name} dashboard URL`).fill('https://new-agent.example.com')
	await page.getByRole('button', { name: 'Save endpoints' }).click()
	await expect.poll(() => savedConfiguration).toMatchObject({
		mode: 'existing_endpoints',
		admin_url: 'https://admin.example.com',
		instance_endpoints: [{ instance_id: instance.id, instance_name: instance.name, dashboard_url: 'https://new-agent.example.com' }],
	})
	await expect(savedConfiguration).not.toHaveProperty('account_id')
})

test('instance dashboard prefers the synchronized public URL while its API remains local', async ({ page }) => {
  const instance = {
    ...codexInstance('model-alpha'),
	public_hostname: 'fleet-codex-sync.example.com',
    public_dashboard_url: 'https://fleet-codex-sync.example.com',
  }
  await page.route('**/api/v1/instances/instance-codex-sync/recovery-points', async (route) => {
    await route.fulfill({ json: [] })
  })
	await page.route('**/api/v1/system/remote-access/configuration', async (route) => {
		await route.fulfill({ json: {
			mode: 'managed_cloudflare',
			admin_tunnel_token_configured: true,
			instances_tunnel_token_configured: true,
			instance_publishing_configured: true,
			instance_publishing_zone: 'example.com',
			legacy_provider_managed: false,
			instance_routes: [{
				instance_id: instance.id,
				instance_name: instance.name,
				hostname: 'fleet-codex-sync.example.com',
				origin_service: 'http://hermes-fleet-instance-fleet-codex-sync-dashboard:9119',
				dns_state: 'ready', route_state: 'ready', endpoint_state: 'reachable',
				provider_state: 'publishing', published: true, revalidating: true,
			}],
		} })
	})
	await openFleet(page, { hosts: [host], instances: [instance], operations: [] })

	const inventoryRow = page.getByRole('row').filter({ hasText: instance.name })
	await expect(inventoryRow.getByRole('link', { name: 'fleet-codex-sync.example.com' })).toHaveAttribute('href', instance.public_dashboard_url)
	const statusTrigger = inventoryRow.getByRole('button', { name: 'View status details: Setup incomplete' })
	await statusTrigger.hover()
	const statusPopover = page.getByRole('dialog', { name: `${instance.name} status details` })
	await expect(statusPopover).toBeVisible()
	await expect(statusPopover).toHaveCSS('position', 'fixed')
	const popoverBox = await statusPopover.boundingBox()
	const viewport = page.viewportSize()
	expect(popoverBox).not.toBeNull()
	expect(viewport).not.toBeNull()
	expect(popoverBox!.x).toBeGreaterThanOrEqual(0)
	expect(popoverBox!.y).toBeGreaterThanOrEqual(0)
	expect(popoverBox!.x + popoverBox!.width).toBeLessThanOrEqual(viewport!.width)
	expect(popoverBox!.y + popoverBox!.height).toBeLessThanOrEqual(viewport!.height)
	await expect(statusPopover.getByText('DNS', { exact: true })).toBeVisible()
	await expect(statusPopover.getByText('Endpoint', { exact: true })).toBeVisible()
	await expect(statusPopover.getByText('Runtime', { exact: true })).toBeVisible()
	await expect(statusPopover.getByText('Readiness', { exact: true })).toBeVisible()

  await page.getByRole('button', { name: instance.name, exact: true }).click()
  await expect(page.getByRole('link', { name: 'Open dashboard' })).toHaveAttribute('href', instance.public_dashboard_url)
  await page.getByRole('button', { name: 'Access' }).click()
	await expect(page.getByRole('heading', { name: 'Instance access' })).toBeVisible()
	await expect(page.getByText('Shared credentials', { exact: true })).toHaveCount(1)
	await expect(page.getByRole('link', { name: instance.public_dashboard_url })).toHaveCount(1)
  await expect(page.getByText('http://127.0.0.1:8650/v1')).toBeVisible()
	await expect(page.getByRole('button', { name: 'Copy Local dashboard' })).toBeVisible()
	await expect(page.getByRole('button', { name: 'Copy Local API' })).toBeVisible()
	await expect(page.getByLabel('Publication health')).not.toBeVisible()
	await page.getByText('Technical details', { exact: true }).click()
	await expect(page.getByLabel('Publication health')).toBeVisible()
	await expect(page.getByText('http://hermes-fleet-instance-fleet-codex-sync-dashboard:9119', { exact: true })).toBeVisible()
	await expect(page.getByRole('button', { name: 'Copy Service URL' })).toBeVisible()
	await expect(page.getByText('Cloudflare', { exact: true })).toBeVisible()
	await expect(page.getByText('Published · Revalidating', { exact: true })).toBeVisible()
	await expect(page.getByLabel('Public hostname')).toHaveCount(0)
	await expect(page.getByRole('button', { name: 'Publish dashboard' })).toHaveCount(0)
	await expect(page.getByRole('button', { name: 'Unpublish' })).toHaveClass(/secondary-button/)
	await expect(page.getByText('DNS, tunnel ingress, and the public endpoint passed verification.')).toHaveCount(0)
	await expect(page.getByText('fleet-codex-sync.example.com', { exact: true })).toBeVisible()
	await expect(page.getByText('Locked while published.', { exact: true })).toBeVisible()
	await expect(page.getByRole('button', { name: 'Edit hostname' })).toHaveCount(0)
	await expect(page.getByRole('button', { name: 'Unpublish' })).toBeVisible()
})

test('Codex recommendation fills an untouched form but never overwrites a dirty choice', async ({ page }) => {
  let overviewReads = 0
  await installBaseRoutes(page)
  await page.route('**/api/v1/instances/instance-codex-sync/recovery-points', async (route) => {
    await route.fulfill({ json: [] })
  })
  await page.route('**/api/v1/overview', async (route) => {
    const responseIndex = Math.min(overviewReads, 2)
    overviewReads += 1
    const instances = [
      codexInstance(undefined),
      codexInstance('model-alpha'),
      codexInstance('model-gamma', ['model-beta', 'model-gamma']),
    ]
    await route.fulfill({ json: { hosts: [host], instances: [instances[responseIndex]], operations: [] } })
  })
  await page.goto('/')
  await navigateToInstances(page)
  await page.getByRole('button', { name: 'fleet-codex-sync' }).click()
  await page.getByRole('button', { name: 'Codex', exact: true }).click()
  const model = page.getByLabel('Model')
  await expect(model).toHaveValue('')

  await page.getByTitle('Refresh instance status').click()
  await expect(model).toHaveValue('model-alpha')

  await model.selectOption('model-beta')
  await page.getByTitle('Refresh instance status').click()
  await expect(model).toHaveValue('model-beta')
  await expect(model.locator('option')).toContainText(['Select model', 'model-beta', 'model-gamma · recommended'])
})

test('System modules keep the active tab visible and backup cards compact on mobile', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.route('**/api/v1/system', async (route) => route.fulfill({ json: {
    fleet_version: '0.11.0', build_id: 'mobile-system', operator_url: 'http://127.0.0.1:9180',
    database_path: '/var/lib/hermes-fleet/fleet.db', backup_retention: 2,
    readiness: { ready: true, last_checked: now },
    capacity: { free_bytes: 10, total_bytes: 20, minimum_free_bytes: 1, minimum_free_percent: 5, operations_safe: true },
    recovery_drill: { status: 'NEVER_RUN', control_plane_backup_checked: false, instance_backups_checked: 0, instances_without_backup: 0 },
    remote_access: { configured: false, state: 'disabled', admin: { routes: 0, synced: false }, instances: { routes: 0, synced: false } },
  } }))
  await page.route('**/api/v1/backups', async (route) => route.fulfill({ json: [
    { id: 'mobile-2', filename: 'hermes-fleet-mobile-backup-2.sqlite', size_bytes: 1024, sha256: 'b'.repeat(64), created_at: now, verified_at: now },
    { id: 'mobile-1', filename: 'hermes-fleet-mobile-backup-1.sqlite', size_bytes: 1024, sha256: 'a'.repeat(64), created_at: now, verified_at: now },
  ] }))
  await openFleet(page, { hosts: [host], instances: [], operations: [] })
  await page.getByTitle('Open navigation').click()
  await page.getByRole('dialog', { name: 'Primary navigation' }).getByRole('button', { name: 'System' }).click()
  await page.getByRole('button', { name: 'Backups & recovery' }).click()

  const tabs = page.getByLabel('System modules')
  const activeTab = tabs.getByRole('button', { name: 'Backups & recovery' })
  const tabIsVisible = await activeTab.evaluate((element) => {
    const tabRect = element.getBoundingClientRect()
    const navRect = element.parentElement!.getBoundingClientRect()
    return tabRect.left >= navRect.left && tabRect.right <= navRect.right
  })
  expect(tabIsVisible).toBe(true)
	await expect(page.getByText('Rolling automatically', { exact: true })).toBeVisible()
	await expect(page.getByRole('button', { name: 'Create backup' })).toBeEnabled()
  const backupRowsAreCompact = await page.locator('.backup-table tbody tr').evaluateAll((rows) => rows.every((row) => row.getBoundingClientRect().height < 230))
  expect(backupRowsAreCompact).toBe(true)
  expect(await page.locator('.backup-section').evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true)
	await page.locator('main').evaluate((element) => element.scrollTo(0, element.scrollHeight))
	const stickyPositions = await page.evaluate(() => ({
		topbar: document.querySelector('.topbar')!.getBoundingClientRect().top,
		tabs: document.querySelector('.system-tabs')!.getBoundingClientRect().top,
		topbarPosition: getComputedStyle(document.querySelector('.topbar')!).position,
		tabsPosition: getComputedStyle(document.querySelector('.system-tabs')!).position,
	}))
	expect(stickyPositions.topbarPosition).toBe('sticky')
	expect(stickyPositions.tabsPosition).toBe('sticky')
	expect(stickyPositions.topbar).toBeGreaterThanOrEqual(0)
	expect(stickyPositions.tabs).toBeGreaterThanOrEqual(69)
	expect(stickyPositions.tabs).toBeLessThanOrEqual(71)
})

test('closed mobile navigation is excluded from keyboard focus and exposes its state', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await openFleet(page, { hosts: [host], instances: [], operations: [] })
  const opener = page.getByTitle('Open navigation')
  const sidebar = page.locator('aside.sidebar')

  await expect(opener).toHaveAttribute('aria-expanded', 'false')
  await opener.focus()
  await page.keyboard.press('Shift+Tab')
  expect(await sidebar.evaluate((element) => element.contains(document.activeElement))).toBe(false)

  await opener.click()
  await expect(opener).toHaveAttribute('aria-expanded', 'true')
  const drawer = page.getByRole('dialog', { name: 'Primary navigation' })
  await expect(drawer).toBeVisible()
  await expect(page.getByTitle('Close navigation')).toBeFocused()
  await page.keyboard.press('Shift+Tab')
  await expect(page.getByRole('button', { name: 'Sign out' })).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(opener).toHaveAttribute('aria-expanded', 'false')
  await expect(opener).toBeFocused()

  await opener.click()
  await page.getByRole('button', { name: 'Hosts' }).click()
  await expect(opener).toHaveAttribute('aria-expanded', 'false')
  await expect(opener).toBeFocused()
})

test('Hosts summarizes readiness and opens an operational host detail drawer', async ({ page }) => {
  const instance = codexInstance('model-alpha')
  const operations = [operation(1, { instance_id: instance.id, summary: 'Restarted managed runtime' })]
  await page.route('**/api/v1/system/runtime-health', async (route) => {
    await route.fulfill({ json: {
      status: 'healthy',
      stream_id: 'stream-hosts',
      state_revision: 1,
      event_subscribers: 1,
      compatibility: {
        control_plane_version: '0.10.0', host_agent_version: host.agent_version,
        runtime_config_schemas: [1], default_job_concurrency: 1, maximum_job_concurrency: 4, features: [],
      },
      queue: {
        pending: 0, active: 0, expired_leases: 0, admission_rejected: false, max_per_host: 1,
        hosts: [{ host_id: host.id, host_name: host.name, pending: 0, active: 0, expired_leases: 0, admission_open: true }],
      },
      metrics: { started_at: now, uptime_seconds: 60, http_requests: 1, http_failures: 0, http_in_flight: 0, duration_samples: 1, average_http_ms: 1, p95_http_ms: 1 },
      components: [], incidents: [],
    } })
  })
  await openFleet(page, { hosts: [host], instances: [instance], operations }, { operations })
  await page.getByRole('button', { name: 'Hosts' }).click()

  const summary = page.getByLabel('Host summary')
  await expect(summary).toContainText('Online1/1')
  await expect(summary).toContainText('Needs attention0')
  await expect(summary).toContainText('Managed instances1')
  await expect(summary).toContainText('Job admission1/1 open')
  await expect(page.getByText('CPU, memory, and host disk telemetry are not reported by the current Host Agent contract.')).toBeVisible()

  await page.setViewportSize({ width: 390, height: 844 })
  const mobileValuesAligned = await page.locator('.host-table td[data-label="Instances"], .host-table td[data-label="Host Agent"], .host-table td[data-label="Admission"], .host-table td[data-label="Status"]').evaluateAll((cells) => cells.every((cell) => {
    const value = cell.querySelector<HTMLElement>('.host-cell-value')
    return Boolean(value && value.getBoundingClientRect().left >= cell.getBoundingClientRect().left + 80)
  }))
  expect(mobileValuesAligned).toBe(true)

  const opener = page.getByRole('button', { name: `Open details for ${host.name}` })
  await opener.click()
  const drawer = page.getByRole('dialog', { name: host.name })
  await expect(drawer).toBeVisible()
  await expect(drawer.getByText('Infrastructure readiness')).toBeVisible()
  await expect(drawer.getByText('Resource capacity telemetry unavailable')).toBeVisible()
  await expect(drawer.getByText('Restarted managed runtime')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(drawer).toBeHidden()
  await expect(opener).toBeFocused()

  instance.observation.status = 'UNKNOWN'
  instance.observation.summary = 'Runtime observation is stale'
  instance.observation.checks = []
  await page.getByTitle('Reload Fleet data').click()
  await expect(summary).toContainText('Needs attention1')
  await expect(page.getByText('Observations stale', { exact: true })).toBeVisible()

  await opener.click()
  await drawer.getByRole('button', { name: new RegExp(instance.name) }).click()
  await expect(page.getByRole('heading', { name: instance.name, level: 1 })).toBeVisible()
})

test('Escape closes the create dialog and restores focus to its opener', async ({ page }) => {
  await openFleet(page, { hosts: [host], instances: [], operations: [] })
  const opener = page.getByRole('button', { name: 'Create instance' })
  await opener.click()
  const dialog = page.getByRole('dialog', { name: 'Create instance' })

  await expect(dialog).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(dialog).toBeHidden()
  await expect(opener).toBeFocused()
})

test('control-plane status becomes unavailable after an overview failure', async ({ page }) => {
  let overviewReads = 0
  await installBaseRoutes(page)
  await page.route('**/api/v1/overview', async (route) => {
    overviewReads += 1
    if (overviewReads === 1) {
      await route.fulfill({ json: { hosts: [host], instances: [], operations: [] } })
      return
    }
    await route.fulfill({ status: 503, json: { error: 'Control plane is unavailable' } })
  })
  await page.goto('/')
  await expect(page.getByText('Control plane online')).toBeVisible()

  await page.getByTitle('Reload Fleet data').click()
  await expect(page.getByText('Control plane is unavailable')).toBeVisible()
  await expect(page.getByText(/Control plane (offline|unavailable)/i)).toBeVisible()
  await expect(page.getByText('Control plane online')).toHaveCount(0)
})

test('runtime repair stays busy and polls the operation by ID until terminal', async ({ page }) => {
  const instance = {
    ...codexInstance('model-alpha'),
    id: 'instance-runtime-repair',
    name: 'fleet-runtime-repair',
    codex_configured: true,
    model: 'model-alpha',
    reasoning: 'medium',
    service_tier: 'normal',
    observation: {
      instance_id: 'instance-runtime-repair',
      hermes_version: '0.19.0',
      status: 'DEGRADED',
      summary: 'Runtime drift detected',
      received_at: now,
      checks: [
        { name: 'runtime', status: 'DRIFT', detail: 'Desired RUNNING state does not match container state or health' },
        { name: 'codex_auth', status: 'OK', detail: 'Codex authentication is connected' },
        { name: 'runtime_configuration', status: 'OK', detail: 'Codex configuration matches Fleet' },
      ],
    },
  }
  await openFleet(page, { hosts: [host], instances: [instance], operations: [] })
  await page.route('**/api/v1/instances/instance-runtime-repair/recovery-points', async (route) => {
    await route.fulfill({ json: [] })
  })
  const queuedOperation = operation(1, {
    id: 'repair-poll-1',
    instance_id: instance.id,
    type: 'REPAIR_RUNTIME',
    status: 'PENDING',
    summary: 'Repair and verify fleet-runtime-repair',
  })
  await page.route('**/api/v1/instances/instance-runtime-repair/actions', async (route) => {
    await route.fulfill({ status: 202, json: queuedOperation })
  })
  let releaseTerminal!: () => void
  const terminalHeld = new Promise<void>((resolve) => {
    releaseTerminal = resolve
  })
  let operationReads = 0
  await page.route('**/api/v1/operations/repair-poll-1', async (route) => {
    operationReads += 1
    if (operationReads === 1) {
      await route.fulfill({ json: { ...queuedOperation, status: 'RUNNING' } })
      return
    }
    await terminalHeld
    await route.fulfill({ json: { ...queuedOperation, status: 'SUCCEEDED', updated_at: new Date(Date.parse(now) + 5_000).toISOString() } })
  })
  await page.route('**/api/v1/instances/instance-runtime-repair/observations/refresh', async (route) => {
    await route.fulfill({ status: 202, json: {} })
  })
  page.on('dialog', async (dialog) => dialog.accept())

  await page.getByRole('button', { name: instance.name }).click()
  await page.getByRole('button', { name: 'Repair and verify', exact: true }).first().click()
  await expect(page.getByRole('button', { name: 'Repairing and verifying' })).toBeDisabled()
  await expect.poll(() => operationReads).toBeGreaterThanOrEqual(1)

  releaseTerminal()
  await expect(page.getByRole('button', { name: 'Repair and verify', exact: true }).first()).toBeEnabled({ timeout: 10_000 })
  expect(operationReads).toBeGreaterThanOrEqual(2)
})

test('failed legacy runtime uses verified recovery instead of provisioning retry', async ({ page }) => {
  const oldImage = 'local/hermes-fleet-runtime:0.19.0-old-wrapper'
  const newImage = 'local/hermes-fleet-runtime:0.19.0-new-wrapper'
  const instance = {
    ...codexInstance('model-alpha'),
    id: 'instance-wrapper-recovery',
    name: 'fleet-wrapper-recovery',
    status: 'FAILED',
    image: oldImage,
    codex_configured: true,
    model: 'model-alpha',
    reasoning: 'medium',
    service_tier: 'normal',
    observation: {
      instance_id: 'instance-wrapper-recovery',
      target_generation: now,
      hermes_version: '0.19.0',
      hermes_source: releases[0].commit,
      status: 'DEGRADED',
      summary: 'Retained runtime requires recovery',
      observed_at: now,
      received_at: now,
      checks: [
        { name: 'runtime', status: 'DRIFT', detail: 'Lifecycle state is FAILED' },
        { name: 'managed_path', status: 'OK', detail: 'Managed instance directory exists' },
        { name: 'manifest', status: 'OK', detail: 'Managed Compose manifest exists' },
        { name: 'environment', status: 'OK', detail: 'Managed environment file exists' },
        { name: 'workspace', status: 'OK', detail: 'Managed workspace directory exists' },
        { name: 'docker_daemon', status: 'OK', detail: 'Docker daemon responded' },
        { name: 'data_volume', status: 'OK', detail: 'Expected Fleet data volume exists' },
        { name: 'containers', status: 'OK', detail: 'Hermes and dashboard containers exist' },
        { name: 'ownership', status: 'OK', detail: 'Container labels match' },
        { name: 'image', status: 'OK', detail: 'Container images match' },
      ],
    },
  }
  await installBaseRoutes(page)
  await page.route('**/api/v1/overview', async (route) => {
    await route.fulfill({ json: { hosts: [host], instances: [instance], operations: [] } })
  })
  await page.route('**/api/v1/instances/instance-wrapper-recovery/recovery-points', async (route) => {
    await route.fulfill({ json: [] })
  })
  let recoveryRequest: Record<string, unknown> | undefined
  await page.route('**/api/v1/instances/instance-wrapper-recovery/hermes-update', async (route) => {
    if (route.request().method() === 'POST') {
      recoveryRequest = route.request().postDataJSON() as Record<string, unknown>
      await route.fulfill({
        status: 202,
        json: operation(1, {
          id: 'wrapper-recovery-operation',
          instance_id: instance.id,
          type: 'UPGRADE_HERMES',
          status: 'PENDING',
          summary: 'Refresh managed Hermes runtime',
        }),
      })
      return
    }
    await route.fulfill({
      json: {
        current_version: '0.19.0',
        current_image: oldImage,
        target_version: '0.19.0',
        target_source: releases[0].commit,
        target_image: newImage,
        official_status: 'CURRENT',
        update_kind: 'RUNTIME_REFRESH',
        official_source: releaseSource,
        official_checked_at: now,
        latest_release: { ...releases[0], image: newImage },
        available: true,
        eligible: true,
        reason: 'Fleet can recover the retained runtime',
      },
    })
  })
  page.on('dialog', async (dialog) => dialog.accept())

  await page.goto('/')
  await navigateToInstances(page)
  await page.getByRole('button', { name: instance.name }).click()
  await expect(page.getByRole('button', { name: 'Recover managed runtime' })).toBeEnabled()
  await page.getByRole('button', { name: 'Recover managed runtime' }).click()
  await expect.poll(() => recoveryRequest).toBeTruthy()
  expect(recoveryRequest).toMatchObject({
    confirm_name: instance.name,
    restore_status: 'RUNNING',
  })
  expect(recoveryRequest?.workflow_id).toMatch(/^[0-9a-f-]{36}$/)
})

test('parallel lifecycle actions on different instances keep independent polling', async ({ page }) => {
  const first = {
    ...codexInstance('model-alpha'),
    id: 'instance-parallel-one',
    name: 'fleet-parallel-one',
    status: 'STOPPED',
    observation: {
      instance_id: 'instance-parallel-one',
      hermes_version: '0.19.0',
      status: 'IN_SYNC',
      summary: 'Stopped',
      received_at: now,
      checks: [],
    },
  }
  const second = {
    ...first,
    id: 'instance-parallel-two',
    name: 'fleet-parallel-two',
    observation: { ...first.observation, instance_id: 'instance-parallel-two' },
  }
  await openFleet(page, { hosts: [host], instances: [first, second], operations: [] })

  const queued = new Map<string, ReturnType<typeof operation>>()
  const actionRequests = new Set<string>()
  await page.route('**/api/v1/instances/*/actions', async (route) => {
    const instanceID = new URL(route.request().url()).pathname.split('/').at(-2) ?? ''
    actionRequests.add(instanceID)
    const item = operation(1, {
      id: `start-${instanceID}`,
      instance_id: instanceID,
      type: 'START',
      status: 'PENDING',
      summary: `Start ${instanceID}`,
    })
    queued.set(item.id, item)
    await route.fulfill({ status: 202, json: item })
  })

  const readCounts = new Map<string, number>()
  const operationReads = new Set<string>()
  await page.route('**/api/v1/operations/start-*', async (route) => {
    const operationID = new URL(route.request().url()).pathname.split('/').at(-1) ?? ''
    operationReads.add(operationID)
    const reads = (readCounts.get(operationID) ?? 0) + 1
    readCounts.set(operationID, reads)
    await route.fulfill({ json: { ...queued.get(operationID), status: reads === 1 ? 'RUNNING' : 'SUCCEEDED' } })
  })

  const firstRow = page.getByRole('row').filter({ hasText: first.name })
  const secondRow = page.getByRole('row').filter({ hasText: second.name })
  await firstRow.getByTitle('Start instance').click()
  await secondRow.getByTitle('Start instance').click()
  await expect.poll(() => actionRequests.size).toBe(2)
  await expect.poll(() => operationReads.size, { timeout: 10_000 }).toBe(2)
  await expect(firstRow.getByTitle('Start instance')).toBeDisabled()
  await expect(secondRow.getByTitle('Start instance')).toBeDisabled()

  await expect(firstRow.getByTitle('Start instance')).toBeEnabled({ timeout: 10_000 })
  await expect(secondRow.getByTitle('Start instance')).toBeEnabled({ timeout: 10_000 })
})

test('Messaging preserves a dirty draft and blocks stale or invalid saves', async ({ page }) => {
  const instance = {
    ...codexInstance('model-alpha'),
    id: 'instance-messaging',
    name: 'fleet-messaging',
    codex_configured: true,
    model: 'model-alpha',
    reasoning: 'medium',
    service_tier: 'normal',
    observation: {
      instance_id: 'instance-messaging',
      hermes_version: '0.19.0',
      status: 'IN_SYNC',
      summary: 'Healthy',
      received_at: now,
      checks: [],
    },
  }
  let savedConfiguration = {
    status: 'APPLIED',
    desired_revision: 'revision-1',
    applied_revision: 'revision-1',
    telegram: {
      enabled: false,
      token_configured: false,
      allowed_users: null,
      group_allowed_users: null,
      group_allowed_chats: null,
      require_mention: true,
      proxy_url: '',
    },
    whatsapp: {
      enabled: false,
      mode: 'bot',
      allowed_users: null,
      unauthorized_dm_behavior: 'ignore',
      reply_prefix: 'Hermes',
    },
  }
  const pageErrors: string[] = []
  page.on('pageerror', (error) => pageErrors.push(error.message))
  await page.route('**/api/v1/instances/instance-messaging/recovery-points', async (route) => {
    await route.fulfill({ json: [] })
  })
  await page.route('**/api/v1/instances/instance-messaging/messaging', async (route) => {
    await route.fulfill({ json: savedConfiguration })
  })
  await openFleet(page, { hosts: [host], instances: [instance], operations: [] })

  await page.getByRole('button', { name: instance.name }).click()
  await page.getByRole('button', { name: 'Messaging' }).click()
  const save = page.getByRole('button', { name: 'Save and apply' })
  await expect(page.getByRole('heading', { name: 'Messaging', exact: true })).toBeVisible()
  await expect(save).toBeDisabled()
  expect(pageErrors).toEqual([])

  const telegram = page.locator('.messaging-channel-card').filter({ hasText: 'Telegram bot' })
  await telegram.getByRole('checkbox', { name: 'Enabled' }).check()
  await telegram.getByLabel('Bot token').fill('123456789:test-token')
  const allowedUsers = page.getByRole('textbox', { name: 'Allowed users', exact: true })
  await allowedUsers.fill('not-a-number')
  await expect(page.getByRole('alert')).toContainText('must be numeric')
  await expect(save).toBeDisabled()

  await allowedUsers.fill('123456789')
  await expect(save).toBeEnabled()
  savedConfiguration = {
    ...savedConfiguration,
    desired_revision: 'revision-2',
    applied_revision: 'revision-2',
    whatsapp: { ...savedConfiguration.whatsapp, reply_prefix: 'Changed elsewhere' },
  }
  await page.getByTitle('Refresh instance status').click()
  await expect(page.getByText('Saved settings changed while you were editing')).toBeVisible()
  await expect(allowedUsers).toHaveValue('123456789')
  await expect(save).toBeDisabled()

  await page.getByRole('button', { name: 'Reload saved settings' }).click()
  await expect(page.getByText('Saved settings changed while you were editing')).toHaveCount(0)
  await expect(page.getByLabel('Reply prefix')).toHaveValue('Changed elsewhere')
  await expect(save).toBeDisabled()
  expect(pageErrors).toEqual([])
})

test('Messaging shows a safe Telegram token hint and opens replacement explicitly', async ({ page }) => {
  const instance = {
    ...codexInstance('model-alpha'),
    id: 'instance-messaging-token',
    name: 'fleet-messaging-token',
    observation: {
      instance_id: 'instance-messaging-token',
      hermes_version: '0.19.0',
      status: 'IN_SYNC',
      summary: 'Healthy',
      received_at: now,
      checks: [],
    },
  }
  await page.route('**/api/v1/instances/instance-messaging-token/recovery-points', async (route) => {
    await route.fulfill({ json: [] })
  })
  await page.route('**/api/v1/instances/instance-messaging-token/messaging', async (route) => {
    await route.fulfill({
      json: {
        status: 'APPLIED',
        desired_revision: 'revision-token',
        applied_revision: 'revision-token',
        telegram: {
          enabled: true,
          token_configured: true,
          token_hint: '123456789:••••••••',
          allowed_users: ['42'],
          group_allowed_users: [],
          group_allowed_chats: [],
          require_mention: true,
          proxy_url: '',
        },
        whatsapp: {
          enabled: false,
          mode: 'bot',
          allowed_users: [],
          unauthorized_dm_behavior: 'ignore',
          reply_prefix: 'Hermes',
        },
      },
    })
  })
  await openFleet(page, { hosts: [host], instances: [instance], operations: [] })

  await page.getByRole('button', { name: instance.name }).click()
  await page.getByRole('button', { name: 'Messaging' }).click()
  const telegram = page.locator('.messaging-channel-card').filter({ hasText: 'Telegram bot' })
  await expect(telegram.getByText('123456789:••••••••')).toBeVisible()
  await expect(telegram.getByLabel('Bot token')).toHaveCount(0)

  await telegram.getByRole('button', { name: 'Replace token' }).click()
  const replacement = telegram.getByLabel('New bot token')
  await expect(replacement).toBeVisible()
  await expect(replacement).toHaveValue('')
  await replacement.fill('123456789:new-secret')
  await telegram.getByRole('button', { name: 'Cancel replacement' }).click()
  await expect(telegram.getByText('123456789:••••••••')).toBeVisible()
  await expect(replacement).toHaveCount(0)
})

test('MCP exposes only redacted remote servers and blocks unsafe drafts', async ({ page }) => {
  const instance = {
    ...codexInstance('model-alpha'),
    id: 'instance-mcp',
    name: 'fleet-mcp',
    observation: {
      instance_id: 'instance-mcp',
      hermes_version: '0.20.0',
      status: 'IN_SYNC',
      summary: 'Healthy',
      received_at: now,
      checks: [],
    },
  }
  await page.route('**/api/v1/instances/instance-mcp/mcp/discover', async (route) => {
    await route.fulfill({
      json: {
        tools: [
          { name: 'fetch', description: 'Fetch a record' },
          { name: 'search', description: 'Search records' },
          { name: 'get_status', description: 'Read service status' },
        ],
      },
    })
  })
  await page.route('**/api/v1/instances/instance-mcp/mcp', async (route) => {
    await route.fulfill({
      json: {
        status: 'APPLIED',
        desired_revision: 'mcp-revision',
        applied_revision: 'mcp-revision',
        applied_at: now,
        servers: [{
          name: 'knowledge',
          source: 'remote',
          url: 'https://mcp.example.com/mcp',
          auth_type: 'bearer',
          token_configured: true,
          token_hint: '••••••••',
          enabled: true,
          tools: ['fetch', 'search'],
        }],
      },
    })
  })
  await openFleet(page, { hosts: [host], instances: [instance], operations: [] })

	await page.getByRole('button', { name: instance.name }).click()
	await page.getByRole('button', { name: 'MCP' }).click()
	await expect(page.getByRole('heading', { name: 'MCP', exact: true })).toBeVisible()
	await expect(page.getByText(/Installed · verified/)).toBeVisible()
  await expect(page.getByText('Local commands, arbitrary package installation, sampling, and server-initiated elicitation are blocked.')).toBeVisible()
  await expect(page.getByText('2 tools')).toBeVisible()
  await expect(page.getByLabel('Remote MCP URL')).toHaveCount(0)
  await expect(page.locator('.mcp-server-card textarea')).toHaveCount(0)

	await page.getByRole('button', { name: 'Configure' }).click()
	await expect(page.getByText('•••••••• is stored encrypted and is not returned.')).toBeVisible()
	await expect(page.getByLabel('Replace bearer token')).toHaveValue('')
	await expect(page.locator('.mcp-server-card textarea')).toHaveCount(0)
	await expect(page.getByRole('checkbox', { name: 'fetch' })).toBeChecked()
	await expect(page.getByRole('checkbox', { name: 'search' })).toBeChecked()

	const endpoint = page.getByLabel('Remote MCP URL')
	await endpoint.fill('https://mcp.example.com/changed')
	await expect(page.getByText('The server name or endpoint changed. Enter a new token to connect.')).toBeVisible()
	await expect(page.getByLabel('Replace bearer token')).toHaveAttribute('aria-invalid', 'true')
	await expect(page.getByRole('button', { name: 'Connect and discover' })).toBeDisabled()
	await page.getByRole('button', { name: 'Cancel changes' }).click()
	await expect(page.getByLabel('Remote MCP URL')).toHaveCount(0)
	await expect(page.getByText('https://mcp.example.com/mcp', { exact: true })).toBeVisible()
	await expect(page.getByRole('alert')).toHaveCount(0)
	await page.getByRole('button', { name: 'Configure' }).click()

	await page.getByRole('button', { name: 'Refresh tools' }).click()
  await expect(page.getByRole('checkbox', { name: 'get_status' })).not.toBeChecked()

  const install = page.getByRole('button', { name: 'Install and verify' })
  await expect(install).toBeDisabled()
  await page.getByRole('button', { name: 'Remove stored token' }).click()
  await expect(page.getByRole('alert')).toContainText('Enter a bearer token for knowledge')
  await expect(install).toBeDisabled()
  await page.getByRole('button', { name: 'Keep stored token' }).click()
  await expect(page.getByRole('alert')).toHaveCount(0)

	await page.getByLabel('Remote MCP URL').fill('http://mcp.example.com/mcp')
  await expect(page.getByRole('alert')).toContainText('HTTPS URL')
  await expect(install).toBeDisabled()

  await page.getByRole('button', { name: 'Add server' }).click()
  await expect(page.locator('.mcp-server-card')).toHaveCount(2)
  await expect(page.getByLabel('Remote MCP URL')).toHaveCount(1)
})

test('MCP failed installation can retry without a synthetic form change', async ({ page }) => {
  const instance = {
    ...codexInstance('model-alpha'),
    id: 'instance-mcp-retry',
    name: 'fleet-mcp-retry',
    observation: {
      instance_id: 'instance-mcp-retry', hermes_version: '0.20.0', status: 'IN_SYNC', summary: 'Healthy', received_at: now, checks: [],
    },
  }
  let retries = 0
  await page.route('**/api/v1/instances/instance-mcp-retry/mcp', async (route) => {
    if (route.request().method() === 'PUT') {
      retries++
      await route.fulfill({
        status: 202,
        json: operation(22, { id: 'mcp-retry-operation', instance_id: instance.id, type: 'CONFIGURE_MCP', status: 'PENDING' }),
      })
      return
    }
    await route.fulfill({
      json: {
        status: 'FAILED', last_error: 'MCP endpoint connection failed', desired_revision: 'mcp-failed-revision',
        servers: [{
          name: 'knowledge', source: 'remote', url: 'https://mcp.example.com/mcp', auth_type: 'none',
          token_configured: false, token_hint: '', enabled: true, tools: ['search'],
        }],
      },
    })
  })
  await openFleet(page, { hosts: [host], instances: [instance], operations: [] })

  await page.getByRole('button', { name: instance.name }).click()
  await page.getByRole('button', { name: 'MCP' }).click()
  const retry = page.getByRole('button', { name: 'Retry installation' })
  await expect(retry).toBeEnabled()
  await expect(page.getByLabel('Remote MCP URL')).toBeVisible()
  await retry.click()
  await expect.poll(() => retries).toBe(1)
})

test('MCP discovery shows the failed stage and retries without editing the connection', async ({ page }) => {
  const instance = {
    ...codexInstance('model-alpha'),
    id: 'instance-mcp-discovery-retry',
    name: 'fleet-mcp-discovery-retry',
    observation: {
      instance_id: 'instance-mcp-discovery-retry', hermes_version: '0.20.0', status: 'IN_SYNC', summary: 'Healthy', received_at: now, checks: [],
    },
  }
  let discoveryAttempts = 0
  await page.route('**/api/v1/instances/instance-mcp-discovery-retry/mcp/discover', async (route) => {
    discoveryAttempts++
    await route.fulfill({
      status: 424,
      json: {
        error: 'The remote MCP server returned HTTP 404.',
        stage: 'Initialize MCP session',
        action: 'Verify the exact MCP URL and confirm its MCP handler is deployed. Replacing the token will not fix HTTP 404.',
        retryable: true,
      },
    })
  })
  await page.route('**/api/v1/instances/instance-mcp-discovery-retry/mcp', async (route) => {
    await route.fulfill({ json: {
      status: 'APPLIED', desired_revision: 'mcp-revision', applied_revision: 'mcp-revision',
      servers: [{
        name: 'knowledge', source: 'remote', url: 'https://mcp.example.com/mcp', auth_type: 'bearer',
        token_configured: true, token_hint: '••••••••', enabled: true, tools: ['search'],
      }],
    } })
  })
  await openFleet(page, { hosts: [host], instances: [instance], operations: [] })

  await page.getByRole('button', { name: instance.name }).click()
  await page.getByRole('button', { name: 'MCP' }).click()
  await page.getByRole('button', { name: 'Configure' }).click()
  await page.getByRole('button', { name: 'Refresh tools' }).click()
	const issue = page.getByRole('alert')
	await expect(issue).toContainText('Initialize MCP session')
	await expect(issue).toContainText('Replacing the token will not fix HTTP 404')
	const retryConnection = page.getByRole('button', { name: 'Retry connection' })
	await expect(retryConnection).toBeEnabled()
	await expect(issue.getByRole('button')).toHaveCount(0)
	await retryConnection.click()
  await expect.poll(() => discoveryAttempts).toBe(2)
})

test('long pages scroll inside the application main region while the header stays visible', async ({ page }) => {
  const operations = Array.from({ length: 25 }, (_, index) => operation(index + 1))
  await openFleet(page, { hosts: [host], instances: [], operations }, { operations })
  await page.getByRole('button', { name: 'Operations' }).click()

  const main = page.locator('main')
  const before = await main.evaluate((element) => ({
    scrollTop: element.scrollTop,
    scrollHeight: element.scrollHeight,
    clientHeight: element.clientHeight,
    overflowY: getComputedStyle(element).overflowY,
  }))
  expect(before.overflowY).toBe('auto')
  expect(before.scrollHeight).toBeGreaterThan(before.clientHeight)
  await main.evaluate((element) => element.scrollTo({ top: element.scrollHeight, behavior: 'instant' }))
  await expect.poll(() => main.evaluate((element) => element.scrollTop)).toBeGreaterThan(0)
  await expect(page.locator('.topbar')).toBeInViewport()
})

test('buffered downloads accept an unknown length safely and reject invalid declared sizes', async ({ page }) => {
  await openFleet(page, { hosts: [host], instances: [], operations: [] })
  const result = await page.evaluate(async () => {
    const moduleURL = new URL('/src/api.ts', window.location.href).href
    const api = await import(moduleURL) as {
      apiDownloadToFile: (token: string, path: string, filename: string) => Promise<void>
    }
    const originalFetch = window.fetch
    const originalClick = HTMLAnchorElement.prototype.click
    const pickerWindow = window as Window & { showSaveFilePicker?: unknown }
    const originalPicker = pickerWindow.showSaveFilePicker
    let clicked = false
    Object.defineProperty(window, 'showSaveFilePicker', { configurable: true, value: undefined })
    HTMLAnchorElement.prototype.click = function click() {
      clicked = true
    }
    try {
      window.fetch = async () => new Response(new Uint8Array([1, 2, 3]), {
        status: 200,
        headers: { 'Content-Type': 'application/octet-stream' },
      })
      await api.apiDownloadToFile('token', '/download', 'backup.bin')

      window.fetch = async () => new Response(new Uint8Array([1]), {
        status: 200,
        headers: { 'Content-Length': 'invalid' },
      })
      let invalidSizeError = ''
      try {
        await api.apiDownloadToFile('token', '/download', 'backup.bin')
      } catch (error) {
        invalidSizeError = error instanceof Error ? error.message : String(error)
      }
      return { clicked, invalidSizeError }
    } finally {
      window.fetch = originalFetch
      HTMLAnchorElement.prototype.click = originalClick
      Object.defineProperty(window, 'showSaveFilePicker', { configurable: true, value: originalPicker })
    }
  })

  expect(result.clicked).toBe(true)
  expect(result.invalidSizeError).toContain('invalid download size')
})
