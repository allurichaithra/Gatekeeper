import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  server: {
    host: '0.0.0.0',
    port: Number(process.env.VITE_PORT ?? 5173),
    proxy: { '/api': process.env.VITE_CONTROL_API ?? 'http://localhost:8081' },
  },
});
