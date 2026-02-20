export const metadata = {
  title: "Sample App",
  description: "A sample Next.js app for Dowse integration tests",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
