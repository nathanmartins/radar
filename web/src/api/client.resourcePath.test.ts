// @vitest-environment jsdom
//
// Cluster-scoped resources have no namespace. Their per-resource URLs send "_"
// in its place: a proxy that merges "//" (Radar Cloud's does) would otherwise
// turn "persistentvolumes//pv-a" into a path no route matches, and the edit or
// delete fails with a 404. A Radar too old to read "_" on the write and audit
// routes keeps getting the empty segment it understands.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useMutation, useQuery } from '@tanstack/react-query'
import type { Capabilities } from '@skyhook-io/k8s-ui'
import {
  fetchResourceWithRelationships,
  useBulkDeleteResources,
  useCascadeDeletePreview,
  useDeleteResource,
  useResourceAudit,
  useUpdateResource,
} from './client'

vi.mock('@tanstack/react-query', async (original) => ({
  ...await original<typeof import('@tanstack/react-query')>(),
  useQuery: vi.fn(() => ({})),
  useMutation: vi.fn(() => ({})),
  useQueryClient: vi.fn(() => ({})),
}))

afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals() })

function connectedRadar(capabilities: Partial<Capabilities> | undefined) {
  vi.mocked(useQuery).mockImplementation(((options: { queryKey: unknown[] }) =>
    options.queryKey[0] === 'capabilities' ? { data: capabilities } : {}) as unknown as typeof useQuery)
}

function captureRequests() {
  const requests: { method: string; path: string }[] = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    requests.push({ method: init?.method ?? 'GET', path: new URL(input, 'http://localhost').pathname })
    return init?.method === 'DELETE'
      ? new Response(null, { status: 204 })
      : new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } })
  }))
  return requests
}

function lastMutationFn<T>(): (vars: T) => Promise<unknown> {
  return vi.mocked(useMutation).mock.calls.at(-1)![0].mutationFn as (vars: T) => Promise<unknown>
}

function lastQueryFn(): () => Promise<unknown> {
  return vi.mocked(useQuery).mock.calls.at(-1)![0].queryFn as () => Promise<unknown>
}

type Ref = { kind: string; namespace: string; name: string }

async function requestsFor(ref: Ref) {
  const { kind, namespace, name } = ref
  const requests = captureRequests()
  await fetchResourceWithRelationships(kind, namespace, name)
  useUpdateResource()
  await lastMutationFn<Parameters<ReturnType<typeof useUpdateResource>['mutateAsync']>[0]>()({
    kind, namespace, name, yaml: `kind: X\nmetadata:\n  name: ${name}\n`,
  })
  useDeleteResource()
  await lastMutationFn<Parameters<ReturnType<typeof useDeleteResource>['mutateAsync']>[0]>()({ kind, namespace, name })
  useBulkDeleteResources()
  await lastMutationFn<Parameters<ReturnType<typeof useBulkDeleteResources>['mutateAsync']>[0]>()({
    items: [{ kind, namespace, name }],
  })
  useCascadeDeletePreview(kind, namespace, name, undefined, true)
  await lastQueryFn()()
  useResourceAudit(kind, namespace, name)
  await lastQueryFn()()
  return requests
}

function expected(ref: Ref, readSegment: string, writeSegment: string) {
  const read = `/api/resources/${ref.kind}/${readSegment}/${ref.name}`
  const write = `/api/resources/${ref.kind}/${writeSegment}/${ref.name}`
  return [
    { method: 'GET', path: read },
    { method: 'PUT', path: write },
    { method: 'DELETE', path: write },
    { method: 'DELETE', path: write },
    { method: 'GET', path: `${read}/cascade-preview` },
    { method: 'GET', path: `/api/audit/resource/${ref.kind}/${writeSegment}/${ref.name}` },
  ]
}

const clusterScoped: Ref[] = [
  { kind: 'persistentvolumes', namespace: '', name: 'pv-a' },
  { kind: 'nodes', namespace: '', name: 'worker-1' },
  { kind: 'clusterroles', namespace: '', name: 'admin' },
  { kind: 'namespaces', namespace: '', name: 'shop' },
  { kind: 'storageclasses', namespace: '', name: 'standard' },
  { kind: 'clusterissuers', namespace: '', name: 'letsencrypt' },
]
const namespaced: Ref = { kind: 'configmaps', namespace: 'shop', name: 'app' }

describe('a Radar that reads "_" as cluster-scoped', () => {
  beforeEach(() => connectedRadar({ features: { clusterScopedPlaceholder: true } }))

  it.each(clusterScoped)('sends "_" for $kind/$name', async (ref) => {
    expect(await requestsFor(ref)).toEqual(expected(ref, '_', '_'))
  })

  it('sends a namespaced resource\'s own namespace', async () => {
    expect(await requestsFor(namespaced)).toEqual(expected(namespaced, 'shop', 'shop'))
  })
})

describe('before capabilities load', () => {
  beforeEach(() => connectedRadar(undefined))

  it.each(clusterScoped)('keeps the empty segment on writes and audit for $kind/$name', async (ref) => {
    expect(await requestsFor(ref)).toEqual(expected(ref, '_', ''))
  })
})

describe('an older Radar without the flag', () => {
  beforeEach(() => connectedRadar({ features: {} }))

  it.each(clusterScoped)('keeps the empty segment on writes and audit for $kind/$name', async (ref) => {
    expect(await requestsFor(ref)).toEqual(expected(ref, '_', ''))
  })

  it('sends a namespaced resource\'s own namespace', async () => {
    expect(await requestsFor(namespaced)).toEqual(expected(namespaced, 'shop', 'shop'))
  })
})
