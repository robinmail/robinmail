import { APP_BASE_HREF } from '@angular/common'
import { type ApplicationConfig, provideBrowserGlobalErrorListeners } from '@angular/core'
import { provideRouter } from '@angular/router'
import { provideWebmail, webmailRoutes } from '@robinmail/webmail'

export const appConfig: ApplicationConfig = {
  providers: [
    provideBrowserGlobalErrorListeners(),
    // The page has no <base> element: the server's policy for it says
    // base-uri 'none'. The router is told where the app lives instead, and
    // index.html names its files with absolute paths (deployUrl).
    { provide: APP_BASE_HREF, useValue: '/' },
    provideRouter(webmailRoutes),
    provideWebmail(),
  ],
}
