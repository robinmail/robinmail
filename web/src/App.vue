<template>
  <UApp :toaster="{ position: 'bottom-right' }" :tooltip="{ delayDuration: 400 }">
    <RouterView />
  </UApp>
</template>
