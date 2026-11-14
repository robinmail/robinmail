import { Component } from '@angular/core'
import { RouterOutlet } from '@angular/router'

// The shell is the router and nothing else: every screen comes from
// @robinmail/webmail.
@Component({
  selector: 'rm-root',
  imports: [RouterOutlet],
  template: '<router-outlet />',
  host: { class: 'block h-full' },
})
export class App {}
