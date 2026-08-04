import { describe, expect, it, vi } from 'vitest'

import { MutationController, type MutationAuthority } from './controller'

const writableIdentity: MutationAuthority = {
  canMutate: true,
  actorPolicy: 'identity',
}

describe('MutationController', () => {
  it('passes the accepted revision as an If-Match precondition', async () => {
    const mutate = vi.fn(async ({ headers }: { headers: Headers }) => {
      expect(headers.get('If-Match')).toBe('"revision-7"')
      return ok({ changed: true })
    })
    const controller = new MutationController({
      authority: () => writableIdentity,
      refresh: vi.fn(async () => true),
    })

    await expect(
      controller.execute({ revision: '"revision-7"', draft: 'Updated title' }, mutate),
    ).resolves.toEqual({ changed: true })
  })

  it('preserves the draft and refreshes authority for a 412 revision conflict', async () => {
    const refresh = vi.fn(async () => true)
    const controller = new MutationController({
      authority: () => writableIdentity,
      refresh,
    })
    const draft = { title: 'Keep this draft' }

    await expect(
      controller.execute({ revision: '"revision-7"', draft }, async () =>
        failure(412, 'revision_conflict', 'The issue changed.'),
      ),
    ).resolves.toBe(false)

    expect(refresh).toHaveBeenCalledOnce()
    expect(controller.state).toMatchObject({
      kind: 'revision-conflict',
      code: 'revision_conflict',
      detail: 'The issue changed.',
      draft,
    })
  })

  it('reports a structured 409 refusal without calling it a revision conflict', async () => {
    const controller = new MutationController({
      authority: () => writableIdentity,
      refresh: vi.fn(async () => true),
    })

    await controller.execute({ draft: 'Move draft' }, async () =>
      failure(409, 'project_archived', 'The destination project is archived.'),
    )

    expect(controller.state).toMatchObject({
      kind: 'domain-error',
      code: 'project_archived',
      detail: 'The destination project is archived.',
      draft: 'Move draft',
    })
  })

  it('invalidates writable authority on a 401 while preserving the draft', async () => {
    const onAuthenticationRequired = vi.fn()
    const options = {
      authority: () => writableIdentity,
      refresh: vi.fn(async () => false),
      onAuthenticationRequired,
    }
    const controller = new MutationController(options)

    await controller.execute({ draft: 'Preserved draft' }, async () =>
      failure(401, 'web_session_required', 'The browser session expired.'),
    )

    expect(onAuthenticationRequired).toHaveBeenCalledOnce()
    expect(controller.state).toMatchObject({ kind: 'blocked', draft: 'Preserved draft' })
  })

  it('refreshes after an uncertain transport result before allowing retry', async () => {
    const order: string[] = []
    const refresh = vi.fn(async () => {
      order.push('refresh')
      return true
    })
    const controller = new MutationController({
      authority: () => writableIdentity,
      refresh,
    })

    await controller.execute({ draft: 'Comment draft' }, async () => {
      order.push('mutate')
      throw new TypeError('connection closed')
    })

    expect(order).toEqual(['mutate', 'refresh'])
    expect(controller.state).toMatchObject({ kind: 'uncertain', draft: 'Comment draft' })
  })

  it('suppresses a duplicate create after the server committed it', async () => {
    const mutate = vi.fn(async () => ok({ changed: true }))
    const controller = new MutationController({
      authority: () => writableIdentity,
      refresh: vi.fn(async () => false),
    })
    const options = { createKey: 'create-example-issue', draft: { title: 'Example issue' } }

    await expect(controller.execute(options, mutate)).resolves.toEqual({ changed: true })
    await expect(controller.execute(options, mutate)).resolves.toEqual({ changed: true })

    expect(mutate).toHaveBeenCalledOnce()
  })

  it.each([
    { canMutate: false, actorPolicy: 'identity', reason: 'stale' },
    { canMutate: false, actorPolicy: 'readonly', reason: 'read-only' },
  ])('fences $reason authority before transport', async (authority) => {
    const mutate = vi.fn(async () => ok({ changed: true }))
    const controller = new MutationController({
      authority: () => authority,
      refresh: vi.fn(async () => true),
    })

    await expect(controller.execute({ draft: 'Draft' }, mutate)).resolves.toBe(false)
    expect(mutate).not.toHaveBeenCalled()
    expect(controller.state.kind).toBe('blocked')
  })

  it('never adds an actor override for an identity session', async () => {
    const controller = new MutationController({
      authority: () => writableIdentity,
      refresh: vi.fn(async () => true),
    })

    await controller.execute({}, async (context) => {
      expect(context.body({ title: 'Updated title' }, 'user-a')).toEqual({
        title: 'Updated title',
      })
      return ok({ changed: true })
    })
  })
})

function ok<T>(data: T) {
  return {
    data,
    response: new Response(JSON.stringify(data), { status: 200 }),
  }
}

function failure(status: number, code: string, detail: string) {
  return {
    error: { error: { code, detail } },
    response: new Response(JSON.stringify({ error: { code, detail } }), { status }),
  }
}
