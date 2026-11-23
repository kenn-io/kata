import { expect, test } from './fixtures'

test('empty task sections stay on one line and share their add action style', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Compact example task',
    metadata: { checklist: [] },
  })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)

  for (const name of ['Checklist', 'Comments']) {
    const section = page.getByRole('region', { name })
    const bounds = await section.boundingBox()
    const heading = await section.locator('header').boundingBox()
    expect(bounds?.height).toBe(heading?.height)
  }
  const checklistAction = page
    .getByRole('region', { name: 'Checklist' })
    .getByRole('button', { name: 'Add item' })
  const linksAction = page
    .getByRole('region', { name: 'Links' })
    .getByRole('button', { name: 'Add' })
  const actionStyle = (element: HTMLElement) => {
    const style = getComputedStyle(element)
    return [style.borderWidth, style.backgroundColor, style.color, style.fontSize, style.height]
  }
  expect(await checklistAction.evaluate(actionStyle)).toEqual(
    await linksAction.evaluate(actionStyle),
  )
})
