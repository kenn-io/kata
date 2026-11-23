const suffix = / \[(member:[0-9a-f-]{36}; enrollment|service-account:[0-9a-f-]{36}; token):[0-9a-f-]{36}\](?![\s\S])/

export function authorName(author: string): string {
  return author.replace(suffix, '')
}
