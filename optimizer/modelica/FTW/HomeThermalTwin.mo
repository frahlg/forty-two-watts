within FTW;
model HomeThermalTwin
  "One-zone home model with heat-pump and site electrical balance"

  parameter Modelica.Units.SI.ThermalConductance heatLossCoefficient = 180
    "Envelope and ventilation heat loss";
  parameter Modelica.Units.SI.HeatCapacity thermalCapacity = 43.2e6
    "Effective zone and building mass heat capacity";
  parameter Modelica.Units.SI.Temperature initialIndoorTemperature = 293.65
    "Initial indoor temperature";
  parameter Real copAtReference(min = 0) = 3.4;
  parameter Real copSlopePerK = 0.05;
  parameter Modelica.Units.SI.Temperature copReferenceTemperature = 280.15;
  parameter Real minimumCOP(min = 0) = 1.5;
  parameter Real maximumCOP(min = minimumCOP) = 5.5;

  Modelica.Blocks.Interfaces.RealInput outsideTemperatureC(unit = "degC")
    "Outdoor dry-bulb temperature";
  Modelica.Blocks.Interfaces.RealInput heatPumpElectricPowerW(
    unit = "W",
    min = 0)
    "Heat-pump electrical load";
  Modelica.Blocks.Interfaces.RealInput disturbanceHeatW(unit = "W")
    "Learned bounded residual heat gain";
  Modelica.Blocks.Interfaces.RealInput nativeLoadW(unit = "W", min = 0)
    "Site load excluding the heat pump";
  Modelica.Blocks.Interfaces.RealInput pvPowerW(unit = "W", max = 0)
    "PV power under the FTW site convention";
  Modelica.Blocks.Interfaces.RealInput batteryPowerW(unit = "W")
    "Positive charge and negative discharge";

  Modelica.Blocks.Interfaces.RealOutput indoorTemperatureC(unit = "degC");
  Modelica.Blocks.Interfaces.RealOutput heatPumpThermalPowerW(unit = "W");
  Modelica.Blocks.Interfaces.RealOutput cop;
  Modelica.Blocks.Interfaces.RealOutput gridPowerW(unit = "W")
    "Positive import and negative export";

  Modelica.Thermal.HeatTransfer.Components.HeatCapacitor zone(
    C = thermalCapacity,
    T(start = initialIndoorTemperature, fixed = true));
  Modelica.Thermal.HeatTransfer.Components.ThermalConductor envelope(
    G = heatLossCoefficient);
  Modelica.Thermal.HeatTransfer.Sources.PrescribedTemperature outsideBoundary;
  Modelica.Thermal.HeatTransfer.Sources.PrescribedHeatFlow heatPumpHeat;
  Modelica.Thermal.HeatTransfer.Sources.PrescribedHeatFlow residualHeat;

equation
  outsideBoundary.T = outsideTemperatureC + 273.15;
  cop = min(
    maximumCOP,
    max(
      minimumCOP,
      copAtReference
        + copSlopePerK
          * (outsideBoundary.T - copReferenceTemperature)));
  heatPumpThermalPowerW = cop * heatPumpElectricPowerW;
  heatPumpHeat.Q_flow = heatPumpThermalPowerW;
  residualHeat.Q_flow = disturbanceHeatW;
  indoorTemperatureC = zone.T - 273.15;
  gridPowerW = nativeLoadW + heatPumpElectricPowerW + pvPowerW + batteryPowerW;

  connect(outsideBoundary.port, envelope.port_a);
  connect(envelope.port_b, zone.port);
  connect(heatPumpHeat.port, zone.port);
  connect(residualHeat.port, zone.port);

  annotation (
    experiment(
      StartTime = 0,
      StopTime = 604800,
      Interval = 300,
      Tolerance = 1e-6),
    Documentation(info = "<html>
<p>This first reference model uses one resistance and one capacitance. Its
parameters map directly to the optimizer's <code>ftw-1r1c-v1</code> model.
The FMU interface keeps electric power in FTW's site convention.</p>
</html>"));
end HomeThermalTwin;
