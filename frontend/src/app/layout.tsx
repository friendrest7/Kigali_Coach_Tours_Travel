import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "Kigali Coach Tours & Travel",
  description: "Explore Rwanda with Kigali Coach Tours & Travel.",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return <html lang="en"><body>{children}</body></html>;
}
