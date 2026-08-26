export type Page = 'overview' | 'routes' | 'models' | 'providers' | 'system'

const pages = new Set<Page>(['overview', 'routes', 'models', 'providers', 'system'])

type PageLocation = Pick<Location, 'href' | 'search'>
type PageEventTarget = Pick<Window, 'location' | 'history' | 'addEventListener' | 'removeEventListener'>

export function pageFromSearch(search: string): Page {
  const candidate = new URLSearchParams(search).get('tab')
  return candidate && pages.has(candidate as Page) ? candidate as Page : 'overview'
}

export function pageURL(page: Page, location: PageLocation): string {
  const url = new URL(location.href)
  url.searchParams.set('tab', page)
  url.searchParams.delete('oauth_status')
  url.searchParams.delete('oauth_message')
  return `${url.pathname}${url.search}${url.hash}`
}

export function writePageHistory(target: Pick<PageEventTarget, 'location' | 'history'>, page: Page, replace = false) {
  const method = replace ? 'replaceState' : 'pushState'
  target.history[method]({}, '', pageURL(page, target.location))
}

export function listenForPageNavigation(target: Pick<PageEventTarget, 'location' | 'addEventListener' | 'removeEventListener'>, onPage: (page: Page) => void) {
  const sync = () => onPage(pageFromSearch(target.location.search))
  target.addEventListener('popstate', sync)
  return () => target.removeEventListener('popstate', sync)
}
