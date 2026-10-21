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
      ['attachment.' + id]: {
        name: 'screen.png',
        media_type: 'image/png',
        size: 12,
        docbank_version: '6f1c2e8a-2222-4222-8222-222222222222',
      },
    },
  })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  const panel = page.getByRole('region', { name: 'Attachments', exact: true })
  await expect(panel).toContainText('screen.png')
  await expect(panel).toContainText('image/png')
  await expect(panel).toContainText('12 bytes')
  await expect(panel.getByRole('button')).toHaveCount(0)
})
