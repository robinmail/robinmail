import './main.css'
import { createApp } from 'vue'
import { addAPIProvider } from '@iconify/vue'
import ui from '@nuxt/ui/vue-plugin'
import App from './App.vue'
import { router } from './router'

// Icons come with the build (see vite.config.ts). An icon that is missing
// from it must not be fetched from the public Iconify API, which is where
// the icon component looks by default: a mail client does not call third
// parties. Pointed at our own origin, a miss is a 404 in the console.
addAPIProvider('', { resources: [`${location.origin}/no-icon-api`] })

createApp(App).use(router).use(ui).mount('#app')
