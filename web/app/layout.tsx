import type { Metadata } from "next";
import { Hanken_Grotesk, JetBrains_Mono } from "next/font/google";
import { AppNav } from "@/components/AppNav";
import "./globals.css";

const hanken = Hanken_Grotesk({
  subsets: ["latin"],
  variable: "--font-hanken",
  display: "swap",
});

const jetbrains = JetBrains_Mono({
  subsets: ["latin"],
  variable: "--font-jetbrains",
  display: "swap",
});

export const metadata: Metadata = {
  title: "Arborette",
  description:
    "Analyze your data — register datasets, ask questions, watch causal triplets stream, browse discovered insights.",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en" className={`${hanken.variable} ${jetbrains.variable}`}>
      <body>
        <div className="mx-auto flex min-h-dvh max-w-6xl flex-col px-5 md:px-8">
          <AppNav />
          <main className="flex-1 pb-20">{children}</main>
          <footer className="border-t border-line py-6 text-xs text-faint">
            Arborette · analyst console
          </footer>
        </div>
      </body>
    </html>
  );
}
