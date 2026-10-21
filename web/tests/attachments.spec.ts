import { expect, test } from './fixtures'

test('attachment references remain readable when Docbank is not configured', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const id = '01JB3Z6V2K8M4Q9T1X5W7Y0RNA'
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Attachment example',
    metadata: {
      attachments: {
        [id]: {
          name: 'screen.png',
          media_type: 'image/png',
          size: 12,
          added_by: 'operator',
          added_at: '2026-10-20T23:10:00Z',
          docbank: {
            vault_uid: '6f1c2e8a-1111-4111-8111-111111111111',
            node_id: 42,
            content_version_id: '6f1c2e8a-2222-4222-8222-222222222222',
            content_sha256: 'a'.repeat(64),
          },
        },
      },
    },
  })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  const panel = page.getByRole('region', { name: 'Attachments', exact: true })
  await expect(panel).toContainText('screen.png')
  await expect(panel).toContainText('image/png')
  await expect(panel).toContainText('12 bytes')
  await panel.getByRole('button', { name: 'Preview screen.png' }).click()
  await expect(panel.getByRole('status')).toHaveText('docbank_not_configured')
})
