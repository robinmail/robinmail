import { Component, type EnvironmentProviders, makeEnvironmentProviders } from '@angular/core'
import type { Routes } from '@angular/router'

@Component({ selector: 'rm-placeholder', template: `<p class="p-4">robinmail</p>` })
class Placeholder {}

export const webmailRoutes: Routes = [{ path: '**', component: Placeholder }]

export function provideWebmail(): EnvironmentProviders {
  return makeEnvironmentProviders([])
}
